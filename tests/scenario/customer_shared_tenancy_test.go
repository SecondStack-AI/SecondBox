//go:build scenario_live

package scenario_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
)

func TestScenarioCustomerSharedTenancyEndToEnd(t *testing.T) {
	fixture := newScenarioFixture(t)
	ensureScenarioRunnerPool(t, fixture)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	spec := scenarioProfileSpec(t, contracts.SandboxDesiredStateRunning)
	spec.Ports = []contracts.PortPolicy{{
		Name: "web", Port: 8080, Protocol: "tcp",
		MaximumSessions: 2, MaximumSessionSeconds: 30,
	}}
	profile := createScenarioProfile(t, fixture, "scenario-customer-shared-tenancy", spec)

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	now := time.Now().UTC()
	tenantARef := secondboxclient.OwnershipRef(uniqueScenarioKey(t, "tenant-a"))
	tenantBRef := secondboxclient.OwnershipRef(uniqueScenarioKey(t, "tenant-b"))
	controllerA, applicationA := createScenarioTenantAuthority(
		t, ctx, fixture, tenantARef, "shared-subject", profile.Name, now.Add(30*time.Minute), nil,
	)
	controllerB, applicationB := createScenarioTenantAuthority(
		t, ctx, fixture, tenantBRef, "shared-subject", profile.Name, now.Add(30*time.Minute), nil,
	)
	clientA := newScenarioApplicationClient(t, fixture, applicationA, tenantARef, "shared-subject")
	clientB := newScenarioApplicationClient(t, fixture, applicationB, tenantBRef, "shared-subject")
	fixtureA := fixture
	fixtureA.subject = clientA
	fixtureB := fixture
	fixtureB.subject = clientB

	handleA, createA := createCustomerSharedSandbox(t, ctx, clientA, profile.Name)
	handleB, createB := createCustomerSharedSandbox(t, ctx, clientB, profile.Name)
	readyA := waitForSandbox(t, ctx, handleA, secondboxclient.SandboxStateReady)
	readyB := waitForSandbox(t, ctx, handleB, secondboxclient.SandboxStateReady)
	waitForScenarioOperation(t, ctx, clientA, createA)
	waitForScenarioOperation(t, ctx, clientB, createB)
	if readyA.ID == readyB.ID || readyA.Metadata["secondbox.dev/name"] != "shared-sandbox" || readyB.Metadata["secondbox.dev/name"] != "shared-sandbox" {
		t.Fatalf("SecondBox same-named tenant Sandboxes = (%#v, %#v)", readyA, readyB)
	}

	assertCustomerSharedReadBoundary(t, ctx, clientA, clientB, readyA.ID, readyB.ID)
	updatedA, err := handleA.UpdateMetadata(ctx, secondboxclient.Metadata{"secondbox.dev/name": "shared-sandbox", "tenant-marker": "a"})
	if err != nil || updatedA.Metadata["tenant-marker"] != "a" {
		t.Fatalf("SecondBox tenant A metadata update = %#v error=%v", updatedA, err)
	}
	assertCustomerSharedMutationDenied(t, ctx, clientA, handleB)

	outcomeA := executeScenarioCommand(t, ctx, handleA, "printf tenant-a", 1024, "tenant-a-exec")
	outcomeB := executeScenarioCommand(t, ctx, handleB, "printf tenant-b", 1024, "tenant-b-exec")
	assertScenarioExited(t, outcomeA, 0, "tenant-a", "")
	assertScenarioExited(t, outcomeB, 0, "tenant-b", "")
	assertCustomerSharedExecDenied(t, ctx, clientA, handleB)

	writeScenarioFile(t, ctx, clientA, handleA, "shared-path.txt", []byte("tenant-a\n"))
	writeScenarioFile(t, ctx, clientB, handleB, "shared-path.txt", []byte("tenant-b\n"))
	if got := string(readScenarioFile(t, ctx, clientA, handleA, "shared-path.txt")); got != "tenant-a\n" {
		t.Fatalf("SecondBox tenant A file = %q", got)
	}
	if got := string(readScenarioFile(t, ctx, clientB, handleB, "shared-path.txt")); got != "tenant-b\n" {
		t.Fatalf("SecondBox tenant B file = %q", got)
	}
	assertCustomerSharedFileDenied(t, ctx, clientA, handleB)

	leaseA := acquireScenarioLease(t, ctx, fixtureA, handleA, 60, "tenant-a-port-lease")
	leaseB := acquireScenarioLease(t, ctx, fixtureB, handleB, 60, "tenant-b-port-lease")
	portA := createScenarioPortSession(t, ctx, fixtureA, handleA, leaseA.ID, "tenant-a-port")
	portB := createScenarioPortSession(t, ctx, fixtureB, handleB, leaseB.ID, "tenant-b-port")
	if portA.ID == portB.ID || portA.State != contracts.PortSessionStateOpen || portB.State != contracts.PortSessionStateOpen {
		t.Fatalf("SecondBox tenant PortSessions = (%#v, %#v)", portA, portB)
	}
	assertCustomerSharedPortDenied(t, ctx, clientA, handleB, leaseB.ID)
	closeCustomerSharedPort(t, ctx, clientA, handleA, portA.ID)
	closeCustomerSharedPort(t, ctx, clientB, handleB, portB.ID)

	assertCustomerSharedDiagnostics(t, ctx, clientA, clientB, readyA.ID, readyB.ID, createA.ID, createB.ID)
	assertCustomerSharedMetrics(t, ctx, fixture, string(tenantARef), string(tenantBRef), applicationA.BearerToken, applicationB.BearerToken)

	scenarioCompose(t, "restart", "--no-deps", "control-plane")
	waitForScenarioControlPlaneReady(t, fixture, 60*time.Second)
	waitForScenarioRunner(t, fixture, 90*time.Second)
	// The restart 409 is documented as retryable: exec admission returns
	// execution_node_unavailable until the Runner control channel and live
	// data-plane routes re-register, so the client retries within a bound.
	reconnectDeadline := time.Now().Add(90 * time.Second)
	for {
		outcome, err := handleB.Execute(ctx, scenarioExecRequest("printf after-control-restart", 1024), uniqueScenarioKey(t, "tenant-b-after-control-restart"), "")
		if err == nil {
			assertScenarioExited(t, outcome, 0, "after-control-restart", "")
			break
		}
		if secondboxclient.ProblemCodeOf(err) != "execution_node_unavailable" {
			t.Fatalf("SecondBox tenant B post-restart Exec: %v", err)
		}
		if time.Now().After(reconnectDeadline) {
			t.Fatalf("SecondBox tenant B execution node did not recover after restart: %v", err)
		}
		time.Sleep(time.Second)
	}

	if _, err := controllerA.RevokeApplicationAuthority(ctx, applicationA.Authority.ID, applicationA.Authority.Revision, uniqueScenarioKey(t, "revoke-tenant-a")); err != nil {
		t.Fatal(err)
	}
	assertCustomerSharedAuthenticationDenied(t, ctx, clientA)
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handleB, "printf after-revocation", 1024, "tenant-b-after-revocation"), 0, "after-revocation", "")

	runnerRunning := true
	t.Cleanup(func() {
		if runnerRunning {
			return
		}
		scenarioCompose(t, "start", "secondbox-runner")
		waitForScenarioRunner(t, fixture, 90*time.Second)
	})
	scenarioCompose(t, "stop", "secondbox-runner")
	runnerRunning = false
	subjectA, err := controllerA.GetSubject(ctx, "shared-subject")
	if err != nil {
		t.Fatal(err)
	}
	closedA, err := controllerA.CloseSubject(ctx, subjectA.Ref, subjectA.Revision, uniqueScenarioKey(t, "close-tenant-a"))
	if err != nil {
		t.Fatal(err)
	}
	cleanupA, err := controllerA.CleanupSubject(ctx, closedA.Ref, closedA.Revision, uniqueScenarioKey(t, "cleanup-tenant-a"))
	if err != nil {
		t.Fatal(err)
	}
	scenarioCompose(t, "restart", "--no-deps", "control-plane")
	waitForScenarioControlPlaneReady(t, fixture, 60*time.Second)
	if page, err := clientB.ListSandboxes(ctx, secondboxclient.SandboxListOptions{}); err != nil || !customerSharedPageContains(page, readyB.ID) {
		t.Fatalf("SecondBox tenant B read while tenant A cleanup is durable = %#v error=%v", page, err)
	}
	scenarioCompose(t, "start", "secondbox-runner")
	runnerRunning = true
	waitForScenarioRunner(t, fixture, 90*time.Second)
	waitForScenarioGenerationReady(t, ctx, handleB, readyB.Generation+1)
	waitForCustomerSharedCleanup(t, ctx, controllerA, "shared-subject", cleanupA.ID)
	assertCustomerSharedQuota(t, ctx, controllerA, "shared-subject", 0)
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handleB, "printf after-runner-reconnect", 1024, "tenant-b-after-runner-reconnect"), 0, "after-runner-reconnect", "")

	expiringSubjectAt := time.Now().UTC().Add(120 * time.Second)
	expiringAuthorityAt := time.Now().UTC().Add(90 * time.Second)
	_, expiringApplication := createScenarioSubjectAuthority(
		t, ctx, controllerA, "expiring-subject", profile.Name, expiringAuthorityAt, &expiringSubjectAt,
	)
	expiringClient := newScenarioApplicationClient(t, fixture, expiringApplication, tenantARef, "expiring-subject")
	expiringHandle, _ := createCustomerSharedSandbox(t, ctx, expiringClient, profile.Name)
	waitForSandbox(t, ctx, expiringHandle, secondboxclient.SandboxStateReady)
	waitForCustomerSharedAuthorityState(t, ctx, controllerA, expiringApplication.Authority.ID, secondboxclient.AuthorityStateExpired)
	assertCustomerSharedAuthenticationDenied(t, ctx, expiringClient)
	waitForCustomerSharedCleanup(t, ctx, controllerA, "expiring-subject", "")
	assertCustomerSharedQuota(t, ctx, controllerA, "expiring-subject", 0)
	assertScenarioExited(t, executeScenarioCommand(t, ctx, handleB, "printf after-expiry", 1024, "tenant-b-after-expiry"), 0, "after-expiry", "")

	cleanupScenarioSandbox(t, clientB, handleB)
	if usage, err := controllerB.GetTenantUsage(ctx); err != nil || usage.Usage.Sandboxes != 0 || usage.Usage.ActiveInstances != 0 || usage.Usage.PortSessions != 0 {
		t.Fatalf("SecondBox tenant B released quota = %#v error=%v", usage, err)
	}
}

func createScenarioTenantAuthority(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	tenantRef secondboxclient.OwnershipRef,
	subjectRef string,
	profile string,
	authorityExpiry time.Time,
	subjectExpiry *time.Time,
) (*secondboxclient.Client, secondboxclient.ApplicationCredentialResponse) {
	t.Helper()
	quota := secondboxclient.TenantQuota{
		MaxSandboxes: 4, MaxActiveInstances: 4, MaxCpuMillis: 4000,
		MaxMemoryBytes: 4 << 30, MaxSnapshots: 4, MaxPortSessions: 4,
		MaxConcurrentOperations: 8, MaxActiveSubjects: 4, MaxApplicationAuthorities: 6,
	}
	if _, err := fixture.admin.CreateTenant(ctx, secondboxclient.CreateTenantRequest{
		Ref: tenantRef, AllowedProfileGrants: []string{profile},
		AllowedApplicationScopes: []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports"},
		AggregateQuota:           quota,
		ExpiryPolicy: secondboxclient.TenantExpiryPolicy{
			MaximumSubjectLifetimeSeconds: 3600, MaximumAuthorityLifetimeSeconds: 3600,
		}, Metadata: map[string]string{"qualification": "customer-shared-tenancy"},
	}, uniqueScenarioKey(t, "create-tenant")); err != nil {
		t.Fatal(err)
	}
	controllerCredential, err := fixture.admin.CreateTenantControllerAuthority(ctx, tenantRef, secondboxclient.CreateTenantControllerAuthorityRequest{
		ExpiresAt: time.Now().UTC().Add(time.Hour), Metadata: map[string]string{"qualification": "customer-shared-tenancy"},
	}, uniqueScenarioKey(t, "create-controller"))
	if err != nil {
		t.Fatal(err)
	}
	controller, err := secondboxclient.NewSecondBoxTenantControllerClient(fixture.baseURL, controllerCredential.BearerToken, fixture.httpClient)
	if err != nil {
		t.Fatal(err)
	}
	_, application := createScenarioSubjectAuthority(t, ctx, controller, subjectRef, profile, authorityExpiry, subjectExpiry)
	return controller, application
}

func createScenarioSubjectAuthority(
	t *testing.T,
	ctx context.Context,
	controller *secondboxclient.Client,
	subjectRef string,
	profile string,
	authorityExpiry time.Time,
	subjectExpiry *time.Time,
) (secondboxclient.Subject, secondboxclient.ApplicationCredentialResponse) {
	t.Helper()
	subject, err := controller.CreateSubject(ctx, secondboxclient.CreateSubjectRequest{
		Ref: secondboxclient.OwnershipRef(subjectRef), ExpiresAt: subjectExpiry,
		Quota: secondboxclient.SubjectQuota{
			MaxSandboxes: 2, MaxActiveInstances: 2, MaxCpuMillis: 2000,
			MaxMemoryBytes: 2 << 30, MaxSnapshots: 2, MaxPortSessions: 2,
			MaxConcurrentOperations: 4,
		}, Metadata: map[string]string{"name": subjectRef},
	}, uniqueScenarioKey(t, "create-subject-"+subjectRef))
	if err != nil {
		t.Fatal(err)
	}
	application, err := controller.CreateApplicationAuthority(ctx, secondboxclient.CreateApplicationAuthorityRequest{
		SubjectRef:    subject.Ref,
		Scopes:        []string{"sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:ports"},
		ProfileGrants: []string{profile}, ExpiresAt: authorityExpiry,
		Metadata: map[string]string{"name": subjectRef},
	}, uniqueScenarioKey(t, "create-application-"+subjectRef))
	if err != nil {
		t.Fatal(err)
	}
	return subject, application
}

func newScenarioApplicationClient(
	t *testing.T,
	fixture scenarioFixture,
	credential secondboxclient.ApplicationCredentialResponse,
	tenantRef secondboxclient.OwnershipRef,
	subjectRef string,
) *secondboxclient.Client {
	t.Helper()
	client, err := secondboxclient.NewSecondBoxSubjectClient(
		fixture.baseURL, credential.BearerToken, string(tenantRef), subjectRef, fixture.httpClient,
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func createCustomerSharedSandbox(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	profile string,
) (*secondboxclient.SandboxHandle, contracts.Operation) {
	t.Helper()
	handle, operation, err := client.CreateSandbox(ctx, secondboxclient.CreateSandboxRequest{
		Profile: secondboxclient.ProfileName(profile), Metadata: map[string]string{"secondbox.dev/name": "shared-sandbox"},
	}, uniqueScenarioKey(t, "create-shared-sandbox"))
	if err != nil {
		t.Fatal(err)
	}
	return handle, operation
}

func assertCustomerSharedReadBoundary(t *testing.T, ctx context.Context, clientA, clientB *secondboxclient.Client, sandboxA, sandboxB string) {
	t.Helper()
	for _, item := range []struct {
		client *secondboxclient.Client
		own    string
		other  string
	}{
		{client: clientA, own: sandboxA, other: sandboxB},
		{client: clientB, own: sandboxB, other: sandboxA},
	} {
		page, err := item.client.ListSandboxes(ctx, secondboxclient.SandboxListOptions{Metadata: map[string]string{"secondbox.dev/name": "shared-sandbox"}})
		if err != nil || !customerSharedPageContains(page, item.own) || customerSharedPageContains(page, item.other) {
			t.Fatalf("SecondBox tenant-scoped Sandbox page = %#v error=%v", page, err)
		}
		_, err = item.client.AdoptSandbox(ctx, item.other)
		assertScenarioAPIError(t, err, http.StatusNotFound, "not_found")
	}
}

func customerSharedPageContains(page secondboxclient.SandboxPage, sandboxID string) bool {
	for _, sandbox := range page.Items {
		if sandbox.ID == sandboxID {
			return true
		}
	}
	return false
}

func assertCustomerSharedMutationDenied(t *testing.T, ctx context.Context, client *secondboxclient.Client, other *secondboxclient.SandboxHandle) {
	t.Helper()
	headers := make(http.Header)
	headers.Set("If-Match", sandboxRevisionETag(other.Snapshot().Revision))
	var sandbox secondboxclient.Sandbox
	err := client.RequestJSON(ctx, "updateSandboxMetadata", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": other.Snapshot().ID}, Headers: headers,
		Body: scenarioBody(t, secondboxclient.UpdateSandboxMetadataRequest{Metadata: map[string]string{"cross-tenant": "denied"}}),
	}, &sandbox)
	assertScenarioAPIError(t, err, http.StatusNotFound, "not_found")
}

func assertCustomerSharedExecDenied(t *testing.T, ctx context.Context, client *secondboxclient.Client, other *secondboxclient.SandboxHandle) {
	t.Helper()
	var outcome secondboxclient.ExecOutcome
	err := client.RequestJSON(ctx, "executeSandboxCommand", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": other.Snapshot().ID},
		Headers:        scenarioDataPlaneHeaders(other, uniqueScenarioKey(t, "cross-tenant-exec")),
		Body:           scenarioBody(t, scenarioExecRequest("true", 1024)),
	}, &outcome)
	assertScenarioAPIError(t, err, http.StatusNotFound, "not_found")
}

func assertCustomerSharedFileDenied(t *testing.T, ctx context.Context, client *secondboxclient.Client, other *secondboxclient.SandboxHandle) {
	t.Helper()
	_, err := client.Request(ctx, "readSandboxFile", secondboxclient.CallOptions{
		PathParameters:  map[string]string{"sandboxId": other.Snapshot().ID},
		QueryParameters: url.Values{"path": []string{"shared-path.txt"}},
		Headers:         other.GenerationHeaders(""),
	})
	assertScenarioAPIError(t, err, http.StatusNotFound, "not_found")
}

func assertCustomerSharedPortDenied(t *testing.T, ctx context.Context, client *secondboxclient.Client, other *secondboxclient.SandboxHandle, leaseID string) {
	t.Helper()
	var session contracts.PortSession
	err := client.RequestJSON(ctx, "createSandboxPortSession", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": other.Snapshot().ID},
		Headers:        scenarioLeaseHeaders(other, leaseID, uniqueScenarioKey(t, "cross-tenant-port")),
		Body:           scenarioBody(t, contracts.CreatePortSessionRequest{Name: "web", DurationSeconds: 30}),
	}, &session)
	assertScenarioAPIError(t, err, http.StatusNotFound, "not_found")
}

func closeCustomerSharedPort(t *testing.T, ctx context.Context, client *secondboxclient.Client, handle *secondboxclient.SandboxHandle, sessionID string) {
	t.Helper()
	scenarioVoid(t, ctx, client, "closeSandboxPortSession", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": handle.Snapshot().ID, "portSessionId": sessionID},
		Headers:        scenarioHeaders(uniqueScenarioKey(t, "close-port")),
	})
}

func assertCustomerSharedDiagnostics(t *testing.T, ctx context.Context, clientA, clientB *secondboxclient.Client, sandboxA, sandboxB, operationA, operationB string) {
	t.Helper()
	var sandboxTiming contracts.SandboxTiming
	if err := clientA.RequestJSON(ctx, "getSandboxTiming", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": sandboxA}, QueryParameters: url.Values{"limit": []string{"20"}},
	}, &sandboxTiming); err != nil || sandboxTiming.SandboxID != sandboxA {
		t.Fatalf("SecondBox tenant A Sandbox timing = %#v error=%v", sandboxTiming, err)
	}
	var operationTiming contracts.OperationTiming
	if err := clientB.RequestJSON(ctx, "getOperationTiming", secondboxclient.CallOptions{
		PathParameters: map[string]string{"operationId": operationB},
	}, &operationTiming); err != nil || operationTiming.OperationID != operationB {
		t.Fatalf("SecondBox tenant B Operation timing = %#v error=%v", operationTiming, err)
	}
	err := clientA.RequestJSON(ctx, "getSandboxTiming", secondboxclient.CallOptions{
		PathParameters: map[string]string{"sandboxId": sandboxB}, QueryParameters: url.Values{"limit": []string{"20"}},
	}, &sandboxTiming)
	assertScenarioAPIError(t, err, http.StatusNotFound, "not_found")
	err = clientB.RequestJSON(ctx, "getOperationTiming", secondboxclient.CallOptions{
		PathParameters: map[string]string{"operationId": operationA},
	}, &operationTiming)
	assertScenarioAPIError(t, err, http.StatusNotFound, "not_found")
}

func assertCustomerSharedMetrics(t *testing.T, ctx context.Context, fixture scenarioFixture, forbidden ...string) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.baseURL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := fixture.httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil || response.StatusCode != http.StatusOK || len(body) == 2<<20 {
		t.Fatalf("SecondBox bounded metrics status=%d bytes=%d error=%v", response.StatusCode, len(body), err)
	}
	for _, value := range forbidden {
		if strings.Contains(string(body), value) {
			t.Fatalf("SecondBox fixed-cardinality metrics disclosed tenant or credential material")
		}
	}
}

func assertCustomerSharedAuthenticationDenied(t *testing.T, ctx context.Context, client *secondboxclient.Client) {
	t.Helper()
	_, err := client.ListSandboxes(ctx, secondboxclient.SandboxListOptions{})
	assertScenarioAPIError(t, err, http.StatusUnauthorized, "authentication_failed")
}

func waitForCustomerSharedCleanup(t *testing.T, ctx context.Context, controller *secondboxclient.Client, subjectRef, operationID string) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		subject, err := controller.GetSubject(ctx, secondboxclient.OwnershipRef(subjectRef))
		if err == nil && subject.CleanupState == secondboxclient.SubjectCleanupStateSucceeded {
			if operationID != "" {
				var operation secondboxclient.Operation
				if err := controller.RequestJSON(ctx, "getOperation", secondboxclient.CallOptions{
					PathParameters: map[string]string{"operationId": operationID},
				}, &operation); err != nil || operation.State != contracts.OperationStateSucceeded {
					t.Fatalf("SecondBox Subject cleanup Operation = %#v error=%v", operation, err)
				}
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SecondBox Subject %s cleanup did not succeed: %v", subjectRef, errors.Join(err, ctx.Err()))
		case <-ticker.C:
		}
	}
}

func waitForCustomerSharedAuthorityState(t *testing.T, ctx context.Context, controller *secondboxclient.Client, authorityID, want string) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		authority, err := controller.GetApplicationAuthority(ctx, secondboxclient.AuthorityID(authorityID))
		if err == nil && authority.State == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SecondBox ApplicationAuthority %s state did not reach %s: %v", authorityID, want, errors.Join(err, ctx.Err()))
		case <-ticker.C:
		}
	}
}

func assertCustomerSharedQuota(t *testing.T, ctx context.Context, controller *secondboxclient.Client, subjectRef string, wantSandboxes int64) {
	t.Helper()
	usage, err := controller.GetTenantUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Usage.Sandboxes != wantSandboxes ||
		usage.Usage.ActiveInstances != 0 ||
		usage.Usage.ActiveSubjects != 0 ||
		usage.Usage.ApplicationAuthorities != 0 ||
		usage.Usage.CPUMillis != 0 ||
		usage.Usage.MemoryBytes != 0 ||
		usage.Usage.Snapshots != 0 ||
		usage.Usage.PortSessions != 0 ||
		usage.Usage.ConcurrentOperations != 0 {
		t.Fatalf("SecondBox Tenant workload usage after Subject cleanup = %#v", usage.Usage)
	}
	found := false
	for _, subject := range usage.Subjects {
		if string(subject.SubjectRef) == subjectRef {
			found = true
			if subject.Usage.Sandboxes != wantSandboxes ||
				subject.Usage.ActiveInstances != 0 ||
				subject.Usage.CPUMillis != 0 ||
				subject.Usage.MemoryBytes != 0 ||
				subject.Usage.Snapshots != 0 ||
				subject.Usage.PortSessions != 0 ||
				subject.Usage.ConcurrentOperations != 0 {
				t.Fatalf("SecondBox Subject %s workload usage after cleanup = %#v", subjectRef, subject.Usage)
			}
		}
	}
	if !found {
		t.Fatalf("SecondBox Tenant usage omitted Subject %s: %#v", subjectRef, usage)
	}
}

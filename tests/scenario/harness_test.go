//go:build scenario_live

package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/SecondStack-AI/SecondBox/pkg/standardresources"
	secondboxclient "github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient"
	scenarioharness "github.com/SecondStack-AI/SecondBox/tests/scenario/harness"
)

const (
	scenarioRunnerPool = standardresources.PoolAMD64
	scenarioRunnerID   = "scenario-runner"
)

// A cleanup delete competes with whatever lifecycle work the control plane
// decided for itself while the test was running, so it reissues against the
// documented retryable conflicts rather than reporting them as failures.
const (
	scenarioCleanupDeleteAttempts = 20
	scenarioCleanupDeleteBackoff  = 250 * time.Millisecond
)

var scenarioKeySequence atomic.Uint64

type scenarioFixture struct {
	baseURL       string
	platformToken string
	admin         *secondboxclient.Client
	subject       *secondboxclient.Client
	httpClient    *http.Client
}

func newScenarioFixture(t *testing.T) scenarioFixture {
	t.Helper()
	baseURL := requireScenarioEnvironment(t, "SECONDBOX_LIVE_BASE_URL")
	platformToken := requireScenarioEnvironment(t, "SECONDBOX_PLATFORM_TOKEN")
	clients, err := scenarioharness.NewClients(
		baseURL,
		platformToken,
		"scenario-tenant",
		"scenario-subject",
		70*time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	return scenarioFixture{
		baseURL:       baseURL,
		platformToken: platformToken,
		admin:         clients.Admin,
		subject:       clients.Subject,
		httpClient:    clients.HTTPClient,
	}
}

func requireScenarioEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("SecondBox scenario requires %s", name)
	}
	return value
}

func scenarioJSON[T any](
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	operationID string,
	options secondboxclient.CallOptions,
) T {
	t.Helper()
	result, err := scenarioharness.RequestJSON[T](ctx, client, operationID, options)
	if err != nil {
		t.Fatalf("SecondBox scenario %s: %v", operationID, err)
	}
	return result
}

func scenarioBody(t *testing.T, value any) io.Reader {
	t.Helper()
	return scenarioharness.JSONBody(value)
}

func scenarioHeaders(idempotencyKey string) http.Header {
	return scenarioharness.IdempotencyHeaders(idempotencyKey)
}

func waitForSandbox(
	t *testing.T,
	ctx context.Context,
	handle *secondboxclient.SandboxHandle,
	states ...secondboxclient.SandboxState,
) secondboxclient.Sandbox {
	t.Helper()
	if len(states) == 0 {
		t.Fatal("SecondBox scenario wait requires terminal states")
	}
	sandbox, err := scenarioharness.WaitSandbox(ctx, handle, states, 55*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return sandbox
}

func decodeScenarioJSON[T any](t *testing.T, response *http.Response) T {
	t.Helper()
	defer response.Body.Close()
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("SecondBox scenario decode response: %v", err)
	}
	return value
}

func TestScenarioDeploymentHealth(t *testing.T) {
	fixture := newScenarioFixture(t)
	for _, endpoint := range []struct {
		path string
		want string
	}{
		{path: "/healthz", want: "healthy"},
		{path: "/readyz", want: "ready"},
	} {
		request, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			fixture.baseURL+endpoint.path,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		response, err := fixture.httpClient.Do(request)
		if err != nil {
			t.Fatalf("SecondBox scenario %s: %v", endpoint.path, err)
		}
		body := decodeScenarioJSON[struct {
			Status string `json:"status"`
		}](t, response)
		if response.StatusCode != http.StatusOK || body.Status != endpoint.want {
			t.Fatalf(
				"SecondBox scenario %s status=%d body=%s, want status=200 body=%s",
				endpoint.path,
				response.StatusCode,
				body.Status,
				endpoint.want,
			)
		}
	}
}

func uniqueScenarioKey(t *testing.T, suffix string) string {
	t.Helper()
	name := strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	return fmt.Sprintf("%s-%s-%d", name, suffix, scenarioKeySequence.Add(1))
}

func ensureScenarioRunnerPool(t *testing.T, fixture scenarioFixture) contracts.RunnerPool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := contracts.CreateRunnerPoolRequest{
		Name:          scenarioRunnerPool,
		State:         contracts.RunnerPoolStateReady,
		Architectures: []string{"amd64"},
		Capabilities: []string{
			"compute",
			"network-policy",
			"storage",
			"cleanup",
			"local-workspace",
			// The operator's statement that this pool serves snapshot-resume
			// Profiles. Without it a snapshot_resume Profile aimed here is a
			// standing incompatibility rather than a shortage, and the control
			// plane refuses it with startup_mode_unsupported before placement.
			"snapshot-resume",
		},
		CapacityPolicy: map[string]int64{
			"maximumInstances": 8,
		},
	}
	var created contracts.RunnerPool
	err := fixture.admin.RequestJSON(ctx, "createRunnerPool", secondboxclient.CallOptions{
		Body: scenarioBody(t, request),
	}, &created)
	if err == nil {
		return created
	}
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) ||
		apiError.StatusCode != http.StatusConflict ||
		apiError.Problem == nil ||
		apiError.Problem.Code != "state_conflict" {
		t.Fatalf("SecondBox scenario createRunnerPool: %v", err)
	}
	return scenarioJSON[contracts.RunnerPool](
		t,
		ctx,
		fixture.admin,
		"getRunnerPool",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"runnerPoolName": scenarioRunnerPool},
		},
	)
}

func scenarioProfileSpec(t *testing.T, initialState string) contracts.ProfileRevisionSpec {
	t.Helper()
	return contracts.ProfileRevisionSpec{
		Pool:                  scenarioRunnerPool,
		Architecture:          "amd64",
		RuntimeBundleDigest:   requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST"),
		ToolchainBundleDigest: requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST"),
		Resources: contracts.ResourcePolicy{
			CPUMillis:            1000,
			MemoryBytes:          256 << 20,
			WorkspaceBytes:       64 << 20,
			ProcessLimit:         128,
			ConcurrentOperations: 2,
		},
		Startup: contracts.StartupPolicy{Mode: contracts.StartupModeColdBoot},
		Lifecycle: contracts.LifecyclePolicy{
			InitialState:           initialState,
			DrainGraceSeconds:      5,
			IdleSeconds:            300,
			MaximumDurationSeconds: 1800,
			LeaseSeconds:           60,
		},
		Retention: contracts.RetentionPolicy{
			SnapshotLimit:            4,
			SnapshotRetentionSeconds: 3600,
		},
		Execution: contracts.ExecutionPolicy{
			MaximumDeadlineMilliseconds: 60000,
			MaximumBufferedOutputBytes:  1 << 20,
			StreamWindowBytes:           65536,
			MaximumTransferBytes:        1 << 20,
			TerminalDetachSeconds:       30,
			DataPlaneTransport:          contracts.DataPlaneTransportProxied,
		},
		Network: contracts.NetworkPolicy{
			Mode:         "deny_all",
			Destinations: []contracts.NetworkDestination{},
		},
		Ports: []contracts.PortPolicy{},
	}
}

func createScenarioProfile(
	t *testing.T,
	fixture scenarioFixture,
	name string,
	spec contracts.ProfileRevisionSpec,
) contracts.Profile {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return scenarioJSON[contracts.Profile](
		t,
		ctx,
		fixture.admin,
		"createProfile",
		secondboxclient.CallOptions{
			Headers: scenarioHeaders("profile-" + name),
			Body: scenarioBody(t, contracts.CreateProfileRequest{
				Name: name,
				Spec: spec,
			}),
		},
	)
}

func createScenarioSandbox(
	t *testing.T,
	fixture scenarioFixture,
	profile contracts.Profile,
	suffix string,
) (*secondboxclient.SandboxHandle, contracts.Operation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	key := uniqueScenarioKey(t, "create-"+suffix)
	request := contracts.CreateSandboxRequest{
		Profile: profile.Name,
		Metadata: map[string]string{
			"scenario": suffix,
		},
	}
	var operation contracts.Operation
	for {
		var err error
		operation, err = scenarioharness.RequestJSON[contracts.Operation](
			ctx,
			fixture.subject,
			"createSandbox",
			secondboxclient.CallOptions{
				Headers: scenarioHeaders(key),
				Body:    scenarioBody(t, request),
			},
		)
		if err == nil {
			break
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) ||
			apiError.Problem == nil ||
			apiError.Problem.Code != "execution_node_unavailable" {
			t.Fatalf("SecondBox scenario createSandbox: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("SecondBox scenario createSandbox remained capacity-blocked: %v", err)
		case <-time.After(250 * time.Millisecond):
		}
	}
	if operation.ID == "" || operation.SandboxID == "" {
		t.Fatalf("SecondBox scenario create operation = %#v", operation)
	}
	sandbox := scenarioJSON[contracts.Sandbox](
		t,
		ctx,
		fixture.subject,
		"getSandbox",
		secondboxclient.CallOptions{
			PathParameters: map[string]string{"sandboxId": operation.SandboxID},
		},
	)
	handle := secondboxclient.NewSandboxHandle(fixture.subject, sandbox)
	t.Cleanup(func() {
		cleanupScenarioSandbox(t, fixture.subject, handle)
	})
	return handle, operation
}

func cleanupScenarioSandbox(
	t *testing.T,
	client *secondboxclient.Client,
	handle *secondboxclient.SandboxHandle,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	sandbox, err := handle.Refresh(ctx)
	if err != nil {
		var apiError *secondboxclient.APIError
		if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
			return
		}
		t.Errorf("SecondBox scenario Sandbox cleanup refresh: %v", err)
		return
	}
	if sandbox.State == contracts.SandboxStateDeleted {
		return
	}
	var operation contracts.Operation
	for attempt := 0; attempt < scenarioCleanupDeleteAttempts; attempt++ {
		operation, err = handle.Delete(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, "cleanup-delete"),
			IfMatch:        sandboxRevisionETag(sandbox.Revision),
		})
		if err == nil {
			break
		}
		if !retryableScenarioCleanupConflict(err) {
			t.Errorf("SecondBox scenario Sandbox cleanup delete: %v", err)
			return
		}
		// A revision conflict clears as soon as the Sandbox is re-read, but a
		// Workspace mutation conflict only clears once the in-flight mutation
		// releases the Workspace mutation slot, so wait between attempts.
		select {
		case <-ctx.Done():
			t.Errorf("SecondBox scenario Sandbox cleanup delete: %v", ctx.Err())
			return
		case <-time.After(scenarioCleanupDeleteBackoff):
		}
		refreshed, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Errorf(
				"SecondBox scenario Sandbox cleanup refresh after conflict: %v",
				refreshErr,
			)
			return
		}
		if refreshed.State == contracts.SandboxStateDeleted {
			return
		}
		sandbox = refreshed
	}
	if err != nil {
		t.Errorf("SecondBox scenario Sandbox cleanup delete remained conflicted: %v", err)
		return
	}
	if _, err := client.WaitOperation(ctx, operation.ID, 100*time.Millisecond); err != nil {
		t.Errorf("SecondBox scenario Sandbox cleanup operation: %v", err)
		return
	}
	waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateDeleted)
}

// retryableScenarioCleanupConflict reports whether a cleanup delete lost a race
// the control plane resolves on its own: the Sandbox revision moved between the
// read and the delete, or a Workspace mutation the control plane dispatched for
// itself still holds the Workspace mutation slot. Both are retryable by
// contract, and `tests/scenario/stress` already classifies them that way.
func retryableScenarioCleanupConflict(err error) bool {
	var apiError *secondboxclient.APIError
	if !errors.As(err, &apiError) {
		return false
	}
	if apiError.StatusCode == http.StatusPreconditionFailed {
		return true
	}
	return apiError.StatusCode == http.StatusConflict &&
		apiError.Problem != nil &&
		apiError.Problem.Code == "workspace_mutation_conflict"
}

func sandboxRevisionETag(revision int64) string {
	return scenarioharness.RevisionETag(revision)
}

func waitForScenarioOperation(
	t *testing.T,
	ctx context.Context,
	client *secondboxclient.Client,
	operation contracts.Operation,
) contracts.Operation {
	t.Helper()
	if strings.TrimSpace(operation.ID) == "" {
		t.Fatalf("SecondBox scenario asynchronous Operation has no ID: %#v", operation)
	}
	terminal, err := client.WaitOperation(ctx, operation.ID, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("SecondBox scenario Operation %s: %v", operation.ID, err)
	}
	if terminal.ID != operation.ID ||
		terminal.State != contracts.OperationStateSucceeded ||
		terminal.CompletedAt == nil {
		t.Fatalf("SecondBox scenario terminal Operation = %#v", terminal)
	}
	return terminal
}

func stopScenarioSandbox(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	key string,
) secondboxclient.Sandbox {
	t.Helper()
	var (
		operation contracts.Operation
		err       error
	)
	for attempt := 0; attempt < 5; attempt++ {
		current, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Fatalf("SecondBox scenario refresh before stop: %v", refreshErr)
		}
		operation, err = handle.Stop(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, key),
			IfMatch:        sandboxRevisionETag(current.Revision),
		})
		if err == nil {
			break
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) ||
			apiError.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("SecondBox scenario stop Sandbox: %v", err)
		}
	}
	if err != nil {
		t.Fatalf("SecondBox scenario stop Sandbox remained revision-conflicted: %v", err)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, operation)
	return waitForSandbox(t, ctx, handle, secondboxclient.SandboxStateStopped)
}

func startScenarioSandbox(
	t *testing.T,
	ctx context.Context,
	fixture scenarioFixture,
	handle *secondboxclient.SandboxHandle,
	key string,
) secondboxclient.Sandbox {
	t.Helper()
	var (
		operation contracts.Operation
		err       error
	)
	for attempt := 0; attempt < 5; attempt++ {
		current, refreshErr := handle.Refresh(ctx)
		if refreshErr != nil {
			t.Fatalf("SecondBox scenario refresh before start: %v", refreshErr)
		}
		operation, err = handle.Start(ctx, secondboxclient.LifecycleOptions{
			IdempotencyKey: uniqueScenarioKey(t, key),
			IfMatch:        sandboxRevisionETag(current.Revision),
		})
		if err == nil {
			break
		}
		var apiError *secondboxclient.APIError
		if !errors.As(err, &apiError) ||
			apiError.StatusCode != http.StatusPreconditionFailed {
			t.Fatalf("SecondBox scenario start Sandbox: %v", err)
		}
	}
	if err != nil {
		t.Fatalf("SecondBox scenario start Sandbox remained revision-conflicted: %v", err)
	}
	waitForScenarioOperation(t, ctx, fixture.subject, operation)
	return waitForSandbox(
		t,
		ctx,
		handle,
		secondboxclient.SandboxStateReady,
		secondboxclient.SandboxStateFailed,
	)
}

func scenarioCompose(t *testing.T, arguments ...string) {
	t.Helper()
	commandArguments := []string{
		"compose",
		"--project-name", requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_COMPOSE_PROJECT"),
		"--file", requireScenarioEnvironment(t, "SECONDBOX_SCENARIO_COMPOSE_FILE"),
	}
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(context.Background(), "docker", commandArguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("SecondBox scenario Compose %v: %v\n%s", arguments, err, output)
	}
}

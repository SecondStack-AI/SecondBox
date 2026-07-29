package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/api"
	"github.com/SecondStack-AI/SecondBox/internal/runnercontrol"
	"github.com/SecondStack-AI/SecondBox/internal/service"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublicBufferedExecAndOrdinaryFilesystemUseDurableRelay(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "data-plane-http")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-data-plane-http")
	scopes := []string{
		"sandbox:read", "sandbox:lifecycle",
		"sandbox:exec", "sandbox:files",
	}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "data-plane-http", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "data-plane-http-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 2 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	dataPlaneService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: dataPlaneService, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)
	fake := newRelayFakeRunner(relay, seed.RunnerID, seed.ConnectionTwo)
	fakeContext, stopFake := context.WithCancel(t.Context())
	defer stopFake()
	fakeErrors := make(chan error, 1)
	go func() { fakeErrors <- fake.run(fakeContext) }()

	stdin := []byte{0, 1, 0xff, 'x'}
	execResponse := dataPlaneJSONRequest(t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/exec", key.Credential, sandbox.Generation, "exec-http-key", map[string]any{
		"command":     map[string]any{"mode": "argv", "executable": "/bin/cat", "arguments": []string{}},
		"environment": map[string]string{}, "stdinBase64": base64.StdEncoding.EncodeToString(stdin),
		"deadlineMilliseconds": 1000, "maximumOutputBytes": 1024,
	})
	assertHTTPStatus(t, execResponse, http.StatusOK)
	var exited contracts.ExecExited
	decodeHTTPJSON(t, execResponse, &exited)
	if exited.Kind != "exited" || exited.ExitCode != 0 ||
		exited.ElapsedMilliseconds != 42 {
		t.Fatalf("Exec outcome = %#v", exited)
	}
	stdout, err := base64.StdEncoding.DecodeString(exited.Output.StdoutBase64)
	if err != nil || !bytes.Equal(stdout, stdin) {
		t.Fatalf("Exec stdout = %v, %v", stdout, err)
	}

	mkdirResponse := dataPlaneJSONRequest(t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/directories", key.Credential, sandbox.Generation, "mkdir-http-key", map[string]any{
		"path": "workspace", "recursive": false,
	})
	assertHTTPStatus(t, mkdirResponse, http.StatusNoContent)
	mkdirResponse.Body.Close()

	content := []byte{0, 2, 0xfe, 'z'}
	contentHash := sha256.Sum256(content)
	digest := "sha-256=:" + base64.StdEncoding.EncodeToString(contentHash[:]) + ":"
	writeRequest, err := http.NewRequest(http.MethodPut, server.URL+"/v1/sandboxes/"+sandbox.ID+"/files?path="+url.QueryEscape("workspace/data.bin"), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, writeRequest, key.Credential, sandbox.Generation, "write-http-key")
	writeRequest.Header.Set("Content-Type", "application/octet-stream")
	writeRequest.Header.Set("Digest", digest)
	writeResponse := doHTTP(t, writeRequest)
	assertHTTPStatus(t, writeResponse, http.StatusOK)
	var writeResult contracts.FileWriteResult
	decodeHTTPJSON(t, writeResponse, &writeResult)
	if writeResult.SHA256 != hex.EncodeToString(contentHash[:]) || writeResult.SizeBytes != int64(len(content)) {
		t.Fatalf("write result = %#v", writeResult)
	}

	readResponse := dataPlaneGET(t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/files?path="+url.QueryEscape("workspace/data.bin"), key.Credential, sandbox.Generation)
	assertHTTPStatus(t, readResponse, http.StatusOK)
	readContent, err := io.ReadAll(readResponse.Body)
	readResponse.Body.Close()
	if err != nil || !bytes.Equal(readContent, content) || readResponse.Header.Get("Digest") != digest {
		t.Fatalf("read = %v digest=%q error=%v", readContent, readResponse.Header.Get("Digest"), err)
	}

	statResponse := dataPlaneGET(t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/files:stat?path="+url.QueryEscape("workspace/data.bin"), key.Credential, sandbox.Generation)
	assertHTTPStatus(t, statResponse, http.StatusOK)
	var stat contracts.FileStat
	decodeHTTPJSON(t, statResponse, &stat)
	if stat.Kind != "file" || stat.SizeBytes != int64(len(content)) || stat.ModifiedAt.IsZero() {
		t.Fatalf("stat = %#v", stat)
	}

	existsResponse := dataPlaneGET(t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/files:exists?path="+url.QueryEscape("workspace/data.bin"), key.Credential, sandbox.Generation)
	assertHTTPStatus(t, existsResponse, http.StatusOK)
	var exists contracts.FileExistsResult
	decodeHTTPJSON(t, existsResponse, &exists)
	if !exists.Exists {
		t.Fatalf("exists = %#v", exists)
	}

	listResponse := dataPlaneGET(t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/directories?path="+url.QueryEscape("workspace"), key.Credential, sandbox.Generation)
	assertHTTPStatus(t, listResponse, http.StatusOK)
	var listing contracts.DirectoryListing
	decodeHTTPJSON(t, listResponse, &listing)
	if len(listing.Entries) != 1 || listing.Entries[0].Path != "workspace/data.bin" ||
		listing.Entries[0].Kind != "file" || listing.Entries[0].SizeBytes != int64(len(content)) ||
		listing.Entries[0].ModifiedAt.IsZero() {
		t.Fatalf("listing = %#v", listing)
	}

	removeRequest := dataPlaneJSONRequestWithMethod(t, http.MethodDelete, server.URL+"/v1/sandboxes/"+sandbox.ID+"/directories", key.Credential, sandbox.Generation, "remove-http-key", map[string]any{
		"path": "workspace/data.bin", "recursive": false, "force": false,
	})
	assertHTTPStatus(t, removeRequest, http.StatusNoContent)
	removeRequest.Body.Close()

	if err := fake.assertObserved(); err != nil {
		t.Fatal(err)
	}
	stopFake()
	select {
	case err := <-fakeErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fake runner did not stop")
	}
}

func TestFlueAdapterCompleteSubsetAgainstRealServiceContract(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	project, account, _ := createProjectAccountAndCredential(t, controlPlane, admin, "flue-real-service")
	profile := createGrantedProfile(t, controlPlane, databaseStore, admin, account, "profile-flue-real-service")
	scopes := []string{
		"sandbox:read", "sandbox:lifecycle",
		"sandbox:exec", "sandbox:files",
	}
	if _, err := updateFixtureServiceAccount(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureUpdateServiceAccountRequest{Scopes: &scopes},
	); err != nil {
		t.Fatal(err)
	}
	key, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, project.ID, account.ID,
		fixtureCreateAPIKeyRequest{Name: "flue-real-service", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	principal := authenticateCredential(t, controlPlane, key.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), principal, "flue-real-service-create",
		contracts.CreateSandboxRequest{Profile: profile.Name, Metadata: map[string]string{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 21, 30, 0, 0, time.UTC)
	seed := seedRelayReadyAssignment(t, sandbox, now)
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 2 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	dataPlaneService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: dataPlaneService, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)
	fake := newRelayFakeRunner(relay, seed.RunnerID, seed.ConnectionTwo)
	fakeContext, stopFake := context.WithCancel(t.Context())
	defer stopFake()
	fakeErrors := make(chan error, 1)
	go func() { fakeErrors <- fake.run(fakeContext) }()

	repositoryRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(
		t.Context(), "node", "--test",
		filepath.Join(repositoryRoot, "tests/integration/flue_real_service.test.ts"),
	)
	command.Dir = repositoryRoot
	command.Env = append(
		os.Environ(),
		"SECONDBOX_FLUE_TEST_BASE_URL="+server.URL,
		"SECONDBOX_FLUE_TEST_PLATFORM_TOKEN="+testPlatformToken,
		"SECONDBOX_FLUE_TEST_TENANT_REF="+principal.TenantRef,
		"SECONDBOX_FLUE_TEST_SUBJECT_REF="+principal.SubjectRef,
		"SECONDBOX_FLUE_TEST_SANDBOX_ID="+sandbox.ID,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real-service Flue contract failed: %v\n%s", err, output)
	}
	if err := fake.assertFlueObserved(now); err != nil {
		t.Fatal(err)
	}

	stopFake()
	select {
	case err := <-fakeErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Flue fake runner did not stop")
	}
}

func TestIndependentProjectsCannotObserveOrMutateAnotherSandbox(t *testing.T) {
	controlPlane, databaseStore := newControlPlaneFixture(t, generousQuota())
	admin := fixtureAdmin(t, controlPlane)
	ownerProject, ownerAccount, _ := createProjectAccountAndCredential(
		t, controlPlane, admin, "isolation-owner",
	)
	otherProject, otherAccount, _ := createProjectAccountAndCredential(
		t, controlPlane, admin, "isolation-other",
	)
	profile := createGrantedProfile(
		t, controlPlane, databaseStore, admin, ownerAccount, "profile-isolation-owner",
	)
	scopes := []string{
		"sandbox:read", "sandbox:lifecycle",
		"sandbox:exec", "sandbox:files",
	}
	for _, identity := range []struct {
		projectID string
		accountID string
		name      string
	}{
		{ownerProject.ID, ownerAccount.ID, "isolation-owner"},
		{otherProject.ID, otherAccount.ID, "isolation-other"},
	} {
		if _, err := updateFixtureServiceAccount(t, controlPlane,
			t.Context(), admin, identity.projectID, identity.accountID,
			fixtureUpdateServiceAccountRequest{Scopes: &scopes},
		); err != nil {
			t.Fatal(err)
		}
	}
	ownerKey, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, ownerProject.ID, ownerAccount.ID,
		fixtureCreateAPIKeyRequest{Name: "isolation-owner", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := createFixtureAPIKey(t, controlPlane,
		t.Context(), admin, otherProject.ID, otherAccount.ID,
		fixtureCreateAPIKeyRequest{Name: "isolation-other", Scopes: scopes},
	)
	if err != nil {
		t.Fatal(err)
	}
	owner := authenticateCredential(t, controlPlane, ownerKey.Credential)
	sandbox, _, err := controlPlane.CreateSandbox(
		t.Context(), owner, "isolation-owner-sandbox",
		contracts.CreateSandboxRequest{
			Profile: profile.Name, Metadata: map[string]string{"secret": "owner-only"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	seedRelayReadyAssignment(t, sandbox, now)
	lease, err := controlPlane.AcquireSandboxLease(
		t.Context(), owner, sandbox.ID, sandbox.Generation, "isolation-owner-lease", 60,
	)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := runnercontrol.NewPostgresFrameRelay(t.Context(), runnercontrol.PostgresFrameRelayConfig{
		DatabaseURL: integrationDatabaseURL, ClaimDuration: 50 * time.Millisecond,
		Retention: time.Hour, MaximumFrameBytes: 1 << 20, MaximumSessionBytes: 2 << 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(relay.Close)
	isolationService, err := service.NewControlPlaneService(service.ControlPlaneConfig{
		Store: databaseStore, PlatformToken: testPlatformToken,
		DefaultSubjectQuota: generousQuota(),
		Now:                 func() time.Time { return now }, NewID: service.NewOpaqueID,
		NewCredentialMaterial: service.NewCredentialMaterial,
		DataPlaneRelay:        relay, DataPlanePollInterval: time.Millisecond,
		PublicBaseURL: "http://secondbox.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _, err := isolationService.CreateSandboxTerminal(
		t.Context(), owner, "isolation-owner-terminal", sandbox.ID,
		sandbox.Generation, lease.ID, "isolation-owner-terminal",
		contracts.CreateTerminalRequest{
			Command:     contracts.ExecCommand{Mode: "shell", Command: "cat"},
			Environment: map[string]string{}, Rows: 24, Columns: 80,
			DeadlineMilliseconds: 30_000, Detachable: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: isolationService, PlatformToken: testPlatformToken, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaximumDataPlaneBodyBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := contractServer(t, handler)
	t.Cleanup(server.Close)

	listRequest, err := http.NewRequest(
		http.MethodGet, server.URL+"/v1/sandboxes?limit=100", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, listRequest, otherKey.Credential)
	listResponse := doHTTP(t, listRequest)
	assertHTTPStatus(t, listResponse, http.StatusOK)
	listBody, err := io.ReadAll(listResponse.Body)
	listResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(listBody, []byte(sandbox.ID)) ||
		bytes.Contains(listBody, []byte("owner-only")) {
		t.Fatalf("cross-Project Sandbox list leaked owner data: %s", listBody)
	}

	inspectRequest, err := http.NewRequest(
		http.MethodGet, server.URL+"/v1/sandboxes/"+sandbox.ID, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, inspectRequest, otherKey.Credential)
	assertHTTPStatusAndClose(t, doHTTP(t, inspectRequest), http.StatusNotFound)

	execResponse := dataPlaneJSONRequest(
		t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/exec",
		otherKey.Credential, sandbox.Generation, "isolation-other-exec",
		map[string]any{
			"command":     map[string]any{"mode": "shell", "command": "cat owner-secret"},
			"environment": map[string]string{}, "deadlineMilliseconds": 1_000,
			"maximumOutputBytes": 1_024,
		},
	)
	assertHTTPStatusAndClose(t, execResponse, http.StatusNotFound)

	readResponse := dataPlaneGET(
		t, server.URL+"/v1/sandboxes/"+sandbox.ID+"/files?path=owner-secret",
		otherKey.Credential, sandbox.Generation,
	)
	assertHTTPStatusAndClose(t, readResponse, http.StatusNotFound)

	terminalRequest, err := http.NewRequest(
		http.MethodGet,
		fmt.Sprintf(
			"%s/v1/sandboxes/%s/terminals/%s",
			server.URL, sandbox.ID, terminal.ID,
		),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(
		t, terminalRequest, otherKey.Credential, sandbox.Generation, "",
	)
	assertHTTPStatusAndClose(t, doHTTP(t, terminalRequest), http.StatusNotFound)

	websocketURL := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/v1/sandboxes/" + sandbox.ID + "/terminals/" + terminal.ID
	headers := make(http.Header)
	setPlatformAuthorizationHeaders(t, headers, otherKey.Credential)
	headers.Set("SecondBox-Generation", fmt.Sprintf("%d", sandbox.Generation))
	headers.Set("Origin", server.URL)
	connection, websocketResponse, websocketErr := (&websocket.Dialer{
		Subprotocols: []string{"secondbox.terminal.v1"},
	}).DialContext(t.Context(), websocketURL, headers)
	if connection != nil {
		connection.Close()
		t.Fatal("cross-Project Terminal WebSocket attachment succeeded")
	}
	if websocketErr == nil || websocketResponse == nil ||
		websocketResponse.StatusCode != http.StatusNotFound {
		t.Fatalf(
			"cross-Project Terminal WebSocket response = %#v error=%v",
			websocketResponse, websocketErr,
		)
	}
	websocketResponse.Body.Close()

	deleteRequest, err := http.NewRequest(
		http.MethodDelete, server.URL+"/v1/sandboxes/"+sandbox.ID, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	setPlatformAuthorization(t, deleteRequest, otherKey.Credential)
	deleteRequest.Header.Set("Idempotency-Key", "isolation-other-delete")
	deleteRequest.Header.Set("If-Match", fmt.Sprintf("\"%d\"", sandbox.Revision))
	assertHTTPStatusAndClose(t, doHTTP(t, deleteRequest), http.StatusNotFound)

	ownerView, err := controlPlane.GetSandbox(t.Context(), owner, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ownerView.TenantRef != ownerProject.ID ||
		ownerView.Metadata["secret"] != "owner-only" ||
		ownerView.DesiredState != contracts.SandboxDesiredStateRunning {
		t.Fatalf("cross-Project attempts changed owner Sandbox: %#v", ownerView)
	}
	pool, err := pgxpool.New(t.Context(), integrationDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var otherProjectSessions int64
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.data_plane_sessions
		WHERE tenant_ref=$1 AND subject_ref=$2`, otherProject.ID, otherAccount.ID,
	).Scan(&otherProjectSessions); err != nil {
		t.Fatal(err)
	}
	if otherProjectSessions != 0 {
		t.Fatalf("cross-Project attempts admitted %d data-plane sessions", otherProjectSessions)
	}
}

type relayFakeRunner struct {
	relay           *runnercontrol.PostgresFrameRelay
	runnerID        string
	connectionID    string
	mu              sync.Mutex
	execStdin       []byte
	mkdirRecursive  *bool
	removeRecursive *bool
	removeForce     *bool
	execOpen        *runnerv1.ExecOpen
	workspaceFiles  map[string][]byte
	directories     map[string]bool
	modifiedAt      map[string]time.Time
	writeAttempts   map[string]int
	exec            map[string]*runnerv1.ExecFrame
	files           map[string]*fakeFileOperation
}

type fakeFileOperation struct {
	frame   *runnerv1.FileFrame
	open    *runnerv1.FileOpen
	content []byte
}

func newRelayFakeRunner(
	relay *runnercontrol.PostgresFrameRelay,
	runnerID string,
	connectionID string,
) *relayFakeRunner {
	return &relayFakeRunner{
		relay: relay, runnerID: runnerID, connectionID: connectionID,
		exec: map[string]*runnerv1.ExecFrame{}, files: map[string]*fakeFileOperation{},
		workspaceFiles: map[string][]byte{}, directories: map[string]bool{".": true},
		modifiedAt: map[string]time.Time{}, writeAttempts: map[string]int{},
	}
}

func (fake *relayFakeRunner) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		now := time.Now().UTC()
		delivery, found, err := fake.relay.ClaimOutboundFrame(ctx, fake.runnerID, fake.connectionID, now)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !found {
			time.Sleep(time.Millisecond)
			continue
		}
		if err := fake.relay.MarkOutboundFrameDelivered(
			ctx, delivery.ID, fake.connectionID, delivery.ClaimAttempt, now,
		); err != nil {
			return err
		}
		if err := fake.handle(ctx, delivery.Message, now); err != nil {
			return err
		}
	}
}

func (fake *relayFakeRunner) handle(ctx context.Context, message *runnerv1.ControlPlaneToRunner, now time.Time) error {
	if frame := message.GetExec(); frame != nil {
		if open := frame.GetOpen(); open != nil {
			fake.exec[frame.OperationId] = frame
			fake.mu.Lock()
			fake.execStdin = bytes.Clone(open.Stdin)
			fake.execOpen = open
			fake.mu.Unlock()
			return nil
		}
		if frame.GetCredit() != nil {
			opened := fake.exec[frame.OperationId]
			if opened == nil {
				return errors.New("fake runner Exec credit has no Open")
			}
			stdout, stderr, exitCode := opened.GetOpen().Stdin, []byte(nil), int32(0)
			if opened.GetOpen().GetShell() == "printf flue" {
				stdout, stderr, exitCode = []byte("flue-out"), []byte("flue-err"), 17
			}
			sequence := uint64(1)
			for _, output := range []struct {
				channel runnerv1.ExecOutputChannel
				data    []byte
			}{
				{runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDOUT, stdout},
				{runnerv1.ExecOutputChannel_EXEC_OUTPUT_CHANNEL_STDERR, stderr},
			} {
				if len(output.data) == 0 {
					continue
				}
				if err := fake.persist(ctx, &runnerv1.RunnerToControlPlane{
					Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
						Fence: opened.Fence, OperationId: frame.OperationId,
						StreamId: frame.StreamId, Sequence: sequence,
						Payload: &runnerv1.ExecFrame_Output{Output: &runnerv1.ExecOutput{
							Channel: output.channel, Data: output.data,
						}},
					}},
				}, now); err != nil {
					return err
				}
				sequence++
			}
			return fake.persist(ctx, &runnerv1.RunnerToControlPlane{
				Message: &runnerv1.RunnerToControlPlane_Exec{Exec: &runnerv1.ExecFrame{
					Fence: opened.Fence, OperationId: frame.OperationId,
					StreamId: frame.StreamId, Sequence: sequence,
					Payload: &runnerv1.ExecFrame_Terminal{Terminal: &runnerv1.ExecTerminal{
						Kind: runnerv1.ExecTerminalKind_EXEC_TERMINAL_KIND_EXITED, ExitCode: exitCode,
						ElapsedMilliseconds: 42,
					}},
				}},
			}, now)
		}
		return nil
	}
	frame := message.GetFile()
	if frame == nil {
		return nil
	}
	if open := frame.GetOpen(); open != nil {
		fake.files[frame.OperationId] = &fakeFileOperation{frame: frame, open: open}
		switch open.Operation {
		case runnerv1.FileOperation_FILE_OPERATION_WRITE, runnerv1.FileOperation_FILE_OPERATION_READ:
			return nil
		case runnerv1.FileOperation_FILE_OPERATION_MKDIR:
			fake.mu.Lock()
			value := open.Recursive
			fake.mkdirRecursive = &value
			parentExists := fake.directories[workspaceParent(open.WorkspaceRelativePath)]
			if open.Recursive {
				fake.createDirectoryTree(open.WorkspaceRelativePath, now)
			} else if parentExists {
				fake.directories[open.WorkspaceRelativePath] = true
				fake.modifiedAt[open.WorkspaceRelativePath] = now
			}
			fake.mu.Unlock()
			if !open.Recursive && !parentExists {
				return fake.fileTerminalKind(
					ctx, frame, 1,
					runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND, now,
				)
			}
			return fake.fileTerminal(ctx, frame, 1, now)
		case runnerv1.FileOperation_FILE_OPERATION_REMOVE:
			fake.mu.Lock()
			recursive, force := open.Recursive, open.Force
			fake.removeRecursive, fake.removeForce = &recursive, &force
			_, fileExists := fake.workspaceFiles[open.WorkspaceRelativePath]
			_, directoryExists := fake.directories[open.WorkspaceRelativePath]
			if fileExists {
				delete(fake.workspaceFiles, open.WorkspaceRelativePath)
				delete(fake.modifiedAt, open.WorkspaceRelativePath)
			}
			if directoryExists && recursive {
				for filePath := range fake.workspaceFiles {
					if pathWithin(open.WorkspaceRelativePath, filePath) {
						delete(fake.workspaceFiles, filePath)
						delete(fake.modifiedAt, filePath)
					}
				}
				for directoryPath := range fake.directories {
					if pathWithin(open.WorkspaceRelativePath, directoryPath) {
						delete(fake.directories, directoryPath)
						delete(fake.modifiedAt, directoryPath)
					}
				}
			}
			fake.mu.Unlock()
			if !fileExists && !directoryExists && !force {
				return fake.fileTerminalKind(
					ctx, frame, 1,
					runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND, now,
				)
			}
			return fake.fileTerminal(ctx, frame, 1, now)
		default:
			return fake.respondFileMetadata(ctx, frame, open, now)
		}
	}
	operation := fake.files[frame.OperationId]
	if operation == nil {
		return errors.New("fake runner File frame has no Open")
	}
	if chunk := frame.GetChunk(); chunk != nil {
		operation.content = append(operation.content, chunk.Data...)
		if uint64(len(operation.content)) == operation.open.ExpectedSize {
			fake.mu.Lock()
			filePath := operation.open.WorkspaceRelativePath
			fake.writeAttempts[filePath]++
			parentExists := fake.directories[workspaceParent(filePath)]
			if parentExists {
				fake.workspaceFiles[filePath] = bytes.Clone(operation.content)
				fake.modifiedAt[filePath] = now
			}
			fake.mu.Unlock()
			if !parentExists {
				return fake.fileTerminalKind(
					ctx, operation.frame, 1,
					runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND, now,
				)
			}
			return fake.fileTerminal(ctx, operation.frame, 1, now)
		}
		return nil
	}
	if frame.GetCredit() != nil {
		return fake.respondFileMetadata(ctx, operation.frame, operation.open, now)
	}
	return nil
}

func (fake *relayFakeRunner) respondFileMetadata(
	ctx context.Context, frame *runnerv1.FileFrame, open *runnerv1.FileOpen, now time.Time,
) error {
	modified := uint64(now.UnixMilli())
	filePath := open.WorkspaceRelativePath
	fake.mu.Lock()
	content, fileExists := fake.workspaceFiles[filePath]
	directoryExists := fake.directories[filePath]
	modifiedAt := fake.modifiedAt[filePath]
	fake.mu.Unlock()
	if !modifiedAt.IsZero() {
		modified = uint64(modifiedAt.UnixMilli())
	}
	metadata := &runnerv1.FileMetadata{Exists: fileExists || directoryExists}
	switch open.Operation {
	case runnerv1.FileOperation_FILE_OPERATION_READ:
		if !fileExists {
			return fake.fileTerminalKind(
				ctx, frame, 1,
				runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND, now,
			)
		}
		hash := sha256.Sum256(content)
		metadata.Size, metadata.Kind, metadata.ModifiedAtUnixMs = uint64(len(content)), runnerv1.FileKind_FILE_KIND_FILE, modified
		metadata.Checksum = "sha256:" + hex.EncodeToString(hash[:])
		if err := fake.fileMetadata(ctx, frame, 1, metadata, now); err != nil {
			return err
		}
		if err := fake.persist(ctx, &runnerv1.RunnerToControlPlane{
			Message: &runnerv1.RunnerToControlPlane_File{File: &runnerv1.FileFrame{
				Fence: frame.Fence, OperationId: frame.OperationId, StreamId: frame.StreamId, Sequence: 2,
				Payload: &runnerv1.FileFrame_Chunk{Chunk: &runnerv1.FileChunk{Data: content}},
			}},
		}, now); err != nil {
			return err
		}
		return fake.fileTerminal(ctx, frame, 3, now)
	case runnerv1.FileOperation_FILE_OPERATION_STAT:
		if !metadata.Exists {
			return fake.fileTerminalKind(
				ctx, frame, 1,
				runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND, now,
			)
		}
		metadata.ModifiedAtUnixMs = modified
		if fileExists {
			metadata.Size, metadata.Kind = uint64(len(content)), runnerv1.FileKind_FILE_KIND_FILE
		} else {
			metadata.Kind = runnerv1.FileKind_FILE_KIND_DIRECTORY
		}
	case runnerv1.FileOperation_FILE_OPERATION_EXISTS:
	case runnerv1.FileOperation_FILE_OPERATION_LIST:
		if !directoryExists {
			return fake.fileTerminalKind(
				ctx, frame, 1,
				runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_NOT_FOUND, now,
			)
		}
		metadata.Kind = runnerv1.FileKind_FILE_KIND_DIRECTORY
		metadata.DirectChildEntries = fake.directChildEntries(filePath)
	}
	if err := fake.fileMetadata(ctx, frame, 1, metadata, now); err != nil {
		return err
	}
	return fake.fileTerminal(ctx, frame, 2, now)
}

func (fake *relayFakeRunner) fileMetadata(ctx context.Context, frame *runnerv1.FileFrame, sequence uint64, metadata *runnerv1.FileMetadata, now time.Time) error {
	return fake.persist(ctx, &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_File{File: &runnerv1.FileFrame{
			Fence: frame.Fence, OperationId: frame.OperationId, StreamId: frame.StreamId, Sequence: sequence,
			Payload: &runnerv1.FileFrame_Metadata{Metadata: metadata},
		}},
	}, now)
}

func (fake *relayFakeRunner) fileTerminal(ctx context.Context, frame *runnerv1.FileFrame, sequence uint64, now time.Time) error {
	return fake.fileTerminalKind(
		ctx, frame, sequence,
		runnerv1.FileTerminalKind_FILE_TERMINAL_KIND_COMPLETED, now,
	)
}

func (fake *relayFakeRunner) fileTerminalKind(
	ctx context.Context,
	frame *runnerv1.FileFrame,
	sequence uint64,
	kind runnerv1.FileTerminalKind,
	now time.Time,
) error {
	return fake.persist(ctx, &runnerv1.RunnerToControlPlane{
		Message: &runnerv1.RunnerToControlPlane_File{File: &runnerv1.FileFrame{
			Fence: frame.Fence, OperationId: frame.OperationId, StreamId: frame.StreamId, Sequence: sequence,
			Payload: &runnerv1.FileFrame_Terminal{Terminal: &runnerv1.FileTerminal{
				Kind: kind,
			}},
		}},
	}, now)
}

func (fake *relayFakeRunner) persist(ctx context.Context, message *runnerv1.RunnerToControlPlane, now time.Time) error {
	_, err := fake.relay.PersistInboundFrame(ctx, runnercontrol.InboundRelayFrame{
		RunnerID: fake.runnerID, ConnectionID: fake.connectionID, Message: message,
	}, now)
	return err
}

func (fake *relayFakeRunner) assertObserved() error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !bytes.Equal(fake.execStdin, []byte{0, 1, 0xff, 'x'}) {
		return fmt.Errorf("fake runner stdin = %v", fake.execStdin)
	}
	if fake.mkdirRecursive == nil || *fake.mkdirRecursive {
		return fmt.Errorf("fake runner mkdir recursive = %v", fake.mkdirRecursive)
	}
	if fake.removeRecursive == nil || *fake.removeRecursive ||
		fake.removeForce == nil || *fake.removeForce {
		return fmt.Errorf("fake runner remove options = %v/%v", fake.removeRecursive, fake.removeForce)
	}
	return nil
}

func (fake *relayFakeRunner) assertFlueObserved(now time.Time) error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.writeAttempts["nested/text.txt"] != 2 ||
		fake.writeAttempts["nested/binary.bin"] != 1 {
		return fmt.Errorf("Flue write attempts = %#v", fake.writeAttempts)
	}
	if fake.mkdirRecursive == nil || !*fake.mkdirRecursive {
		return fmt.Errorf("Flue mkdir recursive = %v", fake.mkdirRecursive)
	}
	if fake.removeRecursive == nil || *fake.removeRecursive ||
		fake.removeForce == nil || *fake.removeForce {
		return fmt.Errorf("Flue remove options = %v/%v", fake.removeRecursive, fake.removeForce)
	}
	if fake.execOpen == nil ||
		fake.execOpen.GetShell() != "printf flue" ||
		fake.execOpen.Cwd != "nested" ||
		fake.execOpen.OutputLimitBytes != 4096 ||
		fake.execOpen.DeadlineUnixMs != uint64(now.Add(321*time.Millisecond).UnixMilli()) {
		return fmt.Errorf("Flue Exec Open = %#v", fake.execOpen)
	}
	if len(fake.execOpen.Environment) != 1 ||
		fake.execOpen.Environment[0].Name != "FLUE_VALUE" ||
		string(fake.execOpen.Environment[0].Value) != "contract" {
		return fmt.Errorf("Flue Exec environment = %#v", fake.execOpen.Environment)
	}
	return nil
}

func (fake *relayFakeRunner) createDirectoryTree(directory string, now time.Time) {
	current := "."
	for _, component := range strings.Split(directory, "/") {
		if component == "" || component == "." {
			continue
		}
		if current == "." {
			current = component
		} else {
			current += "/" + component
		}
		fake.directories[current] = true
		fake.modifiedAt[current] = now
	}
}

func (fake *relayFakeRunner) directChildEntries(
	directory string,
) []*runnerv1.FileMetadataEntry {
	prefix := ""
	if directory != "." {
		prefix = directory + "/"
	}
	entries := make([]*runnerv1.FileMetadataEntry, 0)
	for filePath, content := range fake.workspaceFiles {
		remainder := strings.TrimPrefix(filePath, prefix)
		if remainder == filePath && prefix != "" || strings.Contains(remainder, "/") {
			continue
		}
		entries = append(entries, &runnerv1.FileMetadataEntry{
			Path: filePath, Kind: runnerv1.FileKind_FILE_KIND_FILE,
			Size: uint64(len(content)), ModifiedAtUnixMs: uint64(fake.modifiedAt[filePath].UnixMilli()),
		})
	}
	for directoryPath := range fake.directories {
		if directoryPath == "." || directoryPath == directory {
			continue
		}
		remainder := strings.TrimPrefix(directoryPath, prefix)
		if remainder == directoryPath && prefix != "" || strings.Contains(remainder, "/") {
			continue
		}
		entries = append(entries, &runnerv1.FileMetadataEntry{
			Path: directoryPath, Kind: runnerv1.FileKind_FILE_KIND_DIRECTORY,
			ModifiedAtUnixMs: uint64(fake.modifiedAt[directoryPath].UnixMilli()),
		})
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].Path < entries[right].Path
	})
	return entries
}

func workspaceParent(workspacePath string) string {
	parent := path.Dir(workspacePath)
	if parent == "" || parent == "/" {
		return "."
	}
	return parent
}

func pathWithin(parent string, candidate string) bool {
	return candidate == parent || strings.HasPrefix(candidate, parent+"/")
}

func dataPlaneJSONRequest(t *testing.T, endpoint, credential string, generation int64, idempotencyKey string, body any) *http.Response {
	return dataPlaneJSONRequestWithMethod(t, http.MethodPost, endpoint, credential, generation, idempotencyKey, body)
}

func dataPlaneJSONRequestWithMethod(t *testing.T, method, endpoint, credential string, generation int64, idempotencyKey string, body any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, request, credential, generation, idempotencyKey)
	request.Header.Set("Content-Type", "application/json")
	return doHTTP(t, request)
}

func dataPlaneGET(t *testing.T, endpoint, credential string, generation int64) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	setDataPlaneHeaders(t, request, credential, generation, "")
	return doHTTP(t, request)
}

func setDataPlaneHeaders(t *testing.T, request *http.Request, credential string, generation int64, idempotencyKey string) {
	setPlatformAuthorization(t, request, credential)
	request.Header.Set("SecondBox-Generation", fmt.Sprintf("%d", generation))
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
}

func doHTTP(t *testing.T, request *http.Request) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func assertHTTPStatus(t *testing.T, response *http.Response, expected int) {
	t.Helper()
	if response.StatusCode != expected {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("HTTP status = %d, want %d: %s", response.StatusCode, expected, body)
	}
}

func assertHTTPStatusAndClose(
	t *testing.T,
	response *http.Response,
	expected int,
) {
	t.Helper()
	assertHTTPStatus(t, response, expected)
	response.Body.Close()
}

func decodeHTTPJSON(t *testing.T, response *http.Response, destination any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		t.Fatal(err)
	}
}

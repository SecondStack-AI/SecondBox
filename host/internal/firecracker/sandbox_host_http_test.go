package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestSandboxHostHTTPContractAuthenticatesAndUsesDomainRequests(t *testing.T) {
	runtime := &recordingSandboxHostRuntime{}
	handler, err := NewSandboxHostHTTPHandler(runtime, "host-token", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/ready", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	compute := sandboxHostComputeRequest{
		Environment: sandboxHostEnvironment{
			ID: "env_1", TenantRef: "agent", SubjectRef: "agent_1",
			EnvironmentKey: "compartment_1", WorkspaceID: "wsp_1", CurrentGeneration: 1,
		},
		Workspace: sandboxHostWorkspace{ID: "wsp_1", StorageRef: "workspace:wsp_1"},
		ResourceClass: sandboxHostResourceClass{
			CPUMillis: 2000, MemoryBytes: 4 << 30, DiskBytes: 8 << 30, ProcessLimit: 256,
		},
		Instance: sandboxHostInstance{ID: "ins_1", Generation: 1},
	}
	prepare := sandboxHostRequest(t, handler, "/v1/compute:prepare", sandboxHostEnvelope{
		ContractVersion: sandboxHostContractVersion, Compute: &compute,
	})
	if prepare.Code != http.StatusOK || runtime.prepared.Environment.ID != "env_1" {
		t.Fatalf("prepare status=%d runtime=%#v body=%s", prepare.Code, runtime.prepared, prepare.Body)
	}
	var prepared map[string]string
	if err := json.Unmarshal(prepare.Body.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	start := sandboxHostRequest(t, handler, "/v1/compute:start", sandboxHostEnvelope{
		ContractVersion: sandboxHostContractVersion, Compute: &compute,
		OperationRef: prepared["operationRef"],
	})
	if start.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", start.Code, start.Body)
	}
}

func TestSandboxHostAllowedConnectionIDsFailClosed(t *testing.T) {
	connectionIDs, err := validateAllowedConnectionIDs([]string{"connection-1", "connection-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(connectionIDs) != 2 || connectionIDs[0] != "connection-1" || connectionIDs[1] != "connection-2" {
		t.Fatalf("connection IDs = %#v", connectionIDs)
	}
	for _, values := range [][]string{{""}, {" connection-1"}, {"connection-1", "connection-1"}, make([]string, 101)} {
		if _, err := validateAllowedConnectionIDs(values); err == nil {
			t.Fatalf("expected invalid allowedConnectionIds %#v to fail", values)
		}
	}
}

func TestSandboxHostStopRequiresDurableWorkspaceFreeze(t *testing.T) {
	instanceID := "fc-agent-compartment-stop"
	runtimeInstance := &instance{id: instanceID, done: make(chan struct{})}
	manager := &Manager{
		instances:      map[string]*instance{instanceID: runtimeInstance},
		instancesByKey: map[runtimeInstanceKey]string{},
		guestIPs:       map[string]string{},
		freezeWorkspace: func(context.Context, string) (BackupResponse, error) {
			return BackupResponse{}, errors.New("workspace freeze failed")
		},
	}
	runtime := &firecrackerSandboxHostRuntime{manager: manager}
	err := runtime.Stop(context.Background(), sandboxHostIdentity{
		EnvironmentID: "env_1",
		InstanceID:    "ins_1",
		Generation:    1,
		BackendRef:    instanceID,
	})
	if err == nil || err.Error() != "workspace freeze failed" {
		t.Fatalf("stop error = %v, want workspace freeze failure", err)
	}
	select {
	case <-runtimeInstance.done:
		t.Fatal("instance stopped after workspace freeze failure")
	default:
	}
}

func TestWorkspaceEvidenceClonePreservesSparseAllocation(t *testing.T) {
	const logicalSize = int64(64 << 20)
	const marker = "terminal-workspace-evidence"
	root := t.TempDir()
	sourcePath := filepath.Join(root, "workspace.ext4")
	source, err := os.Create(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Truncate(logicalSize); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteAt([]byte(marker), logicalSize-int64(len(marker))); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	cloneCalled := false
	runtime := &firecrackerSandboxHostRuntime{
		stateRoot: root,
		cloneEvidenceFile: func(destination, source *os.File) error {
			cloneCalled = true
			if err := destination.Truncate(logicalSize); err != nil {
				return err
			}
			content := make([]byte, len(marker))
			if _, err := source.ReadAt(content, logicalSize-int64(len(marker))); err != nil {
				return err
			}
			_, err := destination.WriteAt(content, logicalSize-int64(len(marker)))
			return err
		},
	}
	_, contentHash, size, err := runtime.copyEvidenceFile(sourcePath, "checkpoints")
	if err != nil {
		t.Fatal(err)
	}
	if !cloneCalled {
		t.Fatal("workspace evidence did not use the copy-on-write clone path")
	}
	if size != logicalSize {
		t.Fatalf("workspace evidence size = %d, want %d", size, logicalSize)
	}
	expectedContent, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256(expectedContent)
	if contentHash != hex.EncodeToString(expectedHash[:]) {
		t.Fatalf("workspace evidence hash = %q, want %q", contentHash, hex.EncodeToString(expectedHash[:]))
	}
	evidenceInfo, err := os.Stat(filepath.Join(root, "checkpoints", contentHash))
	if err != nil {
		t.Fatal(err)
	}
	evidenceStat, ok := evidenceInfo.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("workspace evidence allocation metadata is unavailable")
	}
	if allocated := evidenceStat.Blocks * 512; allocated > 1<<20 {
		t.Fatalf("workspace evidence allocated %d bytes for a sparse %d-byte image", allocated, logicalSize)
	}
}

func sandboxHostRequest(t *testing.T, handler http.Handler, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer host-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

type recordingSandboxHostRuntime struct {
	prepared sandboxHostComputeRequest
}

func (*recordingSandboxHostRuntime) Ready(context.Context) error { return nil }
func (r *recordingSandboxHostRuntime) Prepare(_ context.Context, request sandboxHostComputeRequest) (string, error) {
	r.prepared = request
	return "prepare:1", nil
}
func (*recordingSandboxHostRuntime) Start(context.Context, string, sandboxHostComputeRequest) (string, error) {
	return "fc-instance", nil
}
func (*recordingSandboxHostRuntime) Inspect(context.Context, sandboxHostIdentity) (string, error) {
	return "ready", nil
}
func (*recordingSandboxHostRuntime) Stop(context.Context, sandboxHostIdentity) error    { return nil }
func (*recordingSandboxHostRuntime) Destroy(context.Context, sandboxHostIdentity) error { return nil }
func (*recordingSandboxHostRuntime) Purge(context.Context, sandboxHostEnvironment, sandboxHostWorkspace) error {
	return nil
}
func (*recordingSandboxHostRuntime) Checkpoint(context.Context, sandboxHostIdentity) (string, string, int64, error) {
	return "snapshot:1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 4096, nil
}
func (*recordingSandboxHostRuntime) CheckpointWorkspace(context.Context, sandboxHostEnvironment, sandboxHostWorkspace) (string, string, int64, error) {
	return "snapshot:workspace", "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", 4096, nil
}
func (*recordingSandboxHostRuntime) MaterializeWorkspace(context.Context, sandboxHostEnvironment, sandboxHostWorkspace, sandboxHostSnapshot) error {
	return nil
}
func (*recordingSandboxHostRuntime) ExchangeArtifact(context.Context, sandboxHostArtifactInput) (string, int64, string, error) {
	return "artifact:1", 1, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
}
func (*recordingSandboxHostRuntime) Execute(context.Context, sandboxHostExecuteInput) (ToolExecResponse, error) {
	return ToolExecResponse{Stdout: "ok"}, nil
}
func (*recordingSandboxHostRuntime) OpenWorkspaceFile(context.Context, sandboxHostIdentity, string) (io.ReadCloser, int64, error) {
	content := []byte("workspace file")
	return io.NopCloser(bytes.NewReader(content)), int64(len(content)), nil
}
func (*recordingSandboxHostRuntime) PutWorkspaceFile(_ context.Context, _ sandboxHostIdentity, _ string, reader io.Reader) (int64, string, error) {
	content, err := io.ReadAll(reader)
	return int64(len(content)), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", err
}

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"secondstack/sandbox-service/internal/api"
	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/internal/service"
	"secondstack/sandbox-service/internal/store"
	"secondstack/sandbox-service/pkg/contracts"
)

func TestHTTPAPIAuthenticationAndLifecycle(t *testing.T) {
	now := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	var ids atomic.Int64
	coordinator, err := service.NewSandboxService(service.Config{
		Store: store.NewMemoryEnvironmentStore(now), Compute: apiComputeBackend{},
		ExecutionRevoker: apiExecutionRevoker{},
		LeaseTTL:         time.Minute, MaxFileTransferBytes: 1 << 20, Now: func() time.Time { return now },
		NewID: func(prefix string) string { return prefix + "_" + string(rune('a'+ids.Add(1))) },
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewHandler(api.HandlerConfig{
		Service: coordinator, InternalToken: "internal-secret",
		Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxFileTransferBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	health, err := server.Client().Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", health.StatusCode)
	}

	requestBody := contracts.ResolveEnvironmentRequest{
		ContractVersion: contracts.ContractVersionV1,
		TenantRef:       "tenant", SubjectRef: "subject", EnvironmentKey: "environment",
		ImageRef: "image@sha256:test", ToolchainRef: "toolchain:v1",
		ResourceClassID:   contracts.ResourceClassAgentStandard,
		LifecyclePolicyID: contracts.LifecyclePolicyAgentCompartment,
		Metadata:          map[string]string{},
	}
	unauthorized := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/environments:resolve", "", requestBody)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.StatusCode)
	}
	unauthorized.Body.Close()

	resolvedResponse := doJSON(t, server.Client(), http.MethodPost, server.URL+"/v1/environments:resolve", "internal-secret", requestBody)
	defer resolvedResponse.Body.Close()
	if resolvedResponse.StatusCode != http.StatusCreated {
		t.Fatalf("resolve status = %d", resolvedResponse.StatusCode)
	}
	var resolved contracts.ResolveEnvironmentResponse
	if err := json.NewDecoder(resolvedResponse.Body).Decode(&resolved); err != nil {
		t.Fatal(err)
	}
	startResponse := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/environments/"+resolved.Environment.ID+":start", "internal-secret",
		contracts.EnvironmentGenerationRequest{ContractVersion: contracts.ContractVersionV1},
	)
	defer startResponse.Body.Close()
	if startResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(startResponse.Body)
		t.Fatalf("start status = %d body=%s", startResponse.StatusCode, body)
	}
	var started contracts.LifecycleResponse
	if err := json.NewDecoder(startResponse.Body).Decode(&started); err != nil {
		t.Fatal(err)
	}
	if started.Environment.State != contracts.EnvironmentStateReady || started.Instance == nil {
		t.Fatalf("started = %#v", started)
	}
	commitResponse := doJSON(t, server.Client(), http.MethodPost,
		server.URL+"/v1/environments/"+resolved.Environment.ID+"/versions:commit", "internal-secret",
		contracts.CommitWorkspaceVersionRequest{
			ContractVersion:    contracts.ContractVersionV1,
			ExpectedGeneration: started.Environment.CurrentGeneration,
			TerminalTurnID:     "turn-http", TerminalStatus: contracts.WorkspaceTerminalCompleted,
		},
	)
	defer commitResponse.Body.Close()
	if commitResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(commitResponse.Body)
		t.Fatalf("commit status = %d body=%s", commitResponse.StatusCode, body)
	}
	currentRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		server.URL+"/v1/environments/"+resolved.Environment.ID+"/versions:current", nil)
	if err != nil {
		t.Fatal(err)
	}
	currentRequest.Header.Set("Authorization", "Bearer internal-secret")
	currentResponse, err := server.Client().Do(currentRequest)
	if err != nil {
		t.Fatal(err)
	}
	currentResponse.Body.Close()
	if currentResponse.StatusCode != http.StatusOK {
		t.Fatalf("current version status = %d", currentResponse.StatusCode)
	}
	usageRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		server.URL+"/v1/workspace-usage?tenantRef=tenant&subjectRef=subject", nil)
	if err != nil {
		t.Fatal(err)
	}
	usageRequest.Header.Set("Authorization", "Bearer internal-secret")
	usageResponse, err := server.Client().Do(usageRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer usageResponse.Body.Close()
	if usageResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(usageResponse.Body)
		t.Fatalf("workspace usage status = %d body=%s", usageResponse.StatusCode, body)
	}
	var usage contracts.WorkspaceUsage
	if err := json.NewDecoder(usageResponse.Body).Decode(&usage); err != nil {
		t.Fatal(err)
	}
	if usage.EnvironmentCount != 1 || usage.QuotaBytes != 8<<30 || usage.UsageBytes != 4096 {
		t.Fatalf("workspace usage = %#v", usage)
	}
	metricsRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	metricsRequest.Header.Set("Authorization", "Bearer internal-secret")
	metricsResponse, err := server.Client().Do(metricsRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer metricsResponse.Body.Close()
	metrics, err := io.ReadAll(metricsResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"sandbox_instance_starts_ready_total 1",
		"sandbox_retained_workspaces 1",
	} {
		if !bytes.Contains(metrics, []byte(expected)) {
			t.Errorf("metrics missing %q:\n%s", expected, metrics)
		}
	}
	for _, forbidden := range []string{"backend:", "host-secret", "tenant", "subject"} {
		if bytes.Contains(metrics, []byte(forbidden)) {
			t.Errorf("metrics leaked %q:\n%s", forbidden, metrics)
		}
	}
}

func doJSON(t *testing.T, client *http.Client, method, endpoint, token string, value any) *http.Response {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

type apiComputeBackend struct{}

type apiExecutionRevoker struct{}

func (apiExecutionRevoker) RevokeEnvironmentExecutions(context.Context, string) error { return nil }

func (apiComputeBackend) Ready(context.Context) error { return nil }
func (apiComputeBackend) Prepare(_ context.Context, request ports.ComputeRequest) (ports.PreparedCompute, error) {
	return ports.PreparedCompute{OperationRef: "prepare:" + request.Instance.ID}, nil
}
func (apiComputeBackend) Start(_ context.Context, _ ports.PreparedCompute, request ports.ComputeRequest) (ports.RunningCompute, error) {
	return ports.RunningCompute{BackendRef: "backend:" + request.Instance.ID, State: contracts.InstanceStateReady}, nil
}
func (apiComputeBackend) Inspect(context.Context, ports.ComputeIdentity) (ports.ComputeStatus, error) {
	return ports.ComputeStatus{State: contracts.InstanceStateReady}, nil
}
func (apiComputeBackend) Stop(context.Context, ports.ComputeIdentity) error    { return nil }
func (apiComputeBackend) Destroy(context.Context, ports.ComputeIdentity) error { return nil }
func (apiComputeBackend) Purge(context.Context, contracts.Environment, contracts.Workspace) error {
	return nil
}
func (apiComputeBackend) Checkpoint(context.Context, ports.ComputeIdentity) (ports.CheckpointResult, error) {
	return ports.CheckpointResult{
		OpaqueRef:   "snapshot:1",
		ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes:   4096,
	}, nil
}
func (apiComputeBackend) CheckpointWorkspace(context.Context, contracts.Environment, contracts.Workspace) (ports.CheckpointResult, error) {
	return apiComputeBackend{}.Checkpoint(context.Background(), ports.ComputeIdentity{})
}
func (apiComputeBackend) MaterializeWorkspace(context.Context, contracts.Environment, contracts.Workspace, contracts.Snapshot) error {
	return nil
}
func (apiComputeBackend) ExchangeArtifact(context.Context, ports.ArtifactExchangeInput) (ports.ArtifactExchangeResult, error) {
	return ports.ArtifactExchangeResult{
		OpaqueRef: "artifact:1",
		SHA256:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}, nil
}
func (apiComputeBackend) Execute(context.Context, ports.ExecuteInput) (contracts.ExecuteResult, error) {
	return contracts.ExecuteResult{Stdout: "ok"}, nil
}

func (apiComputeBackend) OpenWorkspaceFile(context.Context, ports.WorkspaceFileInput) (io.ReadCloser, int64, error) {
	content := []byte("workspace file")
	return io.NopCloser(bytes.NewReader(content)), int64(len(content)), nil
}

func (apiComputeBackend) PutWorkspaceFile(_ context.Context, _ ports.WorkspaceFileInput, reader io.Reader) (ports.WorkspaceFileWriteResult, error) {
	content, err := io.ReadAll(reader)
	return ports.WorkspaceFileWriteResult{
		SizeBytes: int64(len(content)),
		SHA256:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}, err
}

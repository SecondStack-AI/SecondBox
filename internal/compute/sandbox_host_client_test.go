package compute_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"secondstack/sandbox-service/internal/compute"
	"secondstack/sandbox-service/internal/computeconformance"
	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/pkg/contracts"
)

func TestSandboxHostClientConformance(t *testing.T) {
	computeconformance.Run(t, func(t *testing.T) ports.ComputeBackend {
		t.Helper()
		server := newSandboxHostServer(t)
		t.Cleanup(server.Close)
		client, err := compute.NewSandboxHostClient(server.URL, "host-secret", server.Client())
		if err != nil {
			t.Fatalf("NewSandboxHostClient() error = %v", err)
		}
		return client
	})
}

func TestSandboxHostClientUsesStableDomainRequests(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer host-secret" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeTestJSON(writer, ports.PreparedCompute{OperationRef: "operation:1"})
	}))
	defer server.Close()
	client, err := compute.NewSandboxHostClient(server.URL, "host-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(t.Context(), conformanceComputeRequest())
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ToLower(string(serialized))
	for _, providerTerm := range []string{"firecracker", "kvm", "jailer", "containerid", "vsock"} {
		if strings.Contains(text, providerTerm) {
			t.Fatalf("Sandbox Host request leaked provider term %q: %s", providerTerm, text)
		}
	}
	if body["contractVersion"] != "sandbox-host.secondstack.ai/v1" {
		t.Fatalf("contractVersion = %#v", body["contractVersion"])
	}
	computeBody, ok := body["compute"].(map[string]any)
	if !ok {
		t.Fatalf("compute = %#v", body["compute"])
	}
	for _, field := range []string{"environment", "workspace", "resourceClass", "instance"} {
		if _, ok := computeBody[field]; !ok {
			t.Fatalf("compute.%s is missing from %#v", field, computeBody)
		}
	}
	for _, field := range []string{"environment", "workspace", "resourceClass", "instance"} {
		value, ok := computeBody[field].(map[string]any)
		if !ok {
			t.Fatalf("compute.%s = %#v", field, computeBody[field])
		}
		if _, ok := value["contractVersion"]; ok {
			t.Fatalf("compute.%s leaked the Sandbox Service contract version: %#v", field, value)
		}
	}
}

func TestSandboxHostClientProjectsExecuteOperation(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		writeTestJSON(writer, contracts.ExecuteResult{Stdout: "ok", ExitCode: 0})
	}))
	defer server.Close()
	client, err := compute.NewSandboxHostClient(server.URL, "host-secret", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Execute(t.Context(), ports.ExecuteInput{
		Identity: ports.ComputeIdentity{
			EnvironmentID: "env",
			InstanceID:    "ins",
			Generation:    1,
			BackendRef:    "backend",
		},
		Operation: contracts.ExecuteRequest{
			ContractVersion:      contracts.ContractVersionV1,
			ExpectedGeneration:   1,
			LeaseID:              "lea",
			Operation:            "exec",
			Command:              "printf",
			Args:                 []string{"ok"},
			AllowedConnectionIDs: []string{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	executeBody, ok := body["execute"].(map[string]any)
	if !ok {
		t.Fatalf("execute = %#v", body["execute"])
	}
	operation, ok := executeBody["operation"].(map[string]any)
	if !ok {
		t.Fatalf("execute.operation = %#v", executeBody["operation"])
	}
	for _, field := range []string{"contractVersion", "expectedGeneration", "leaseId"} {
		if _, ok := operation[field]; ok {
			t.Fatalf("execute.operation leaked Sandbox Service field %q: %#v", field, operation)
		}
	}
	if operation["operation"] != "exec" || operation["command"] != "printf" {
		t.Fatalf("execute.operation lost host execution intent: %#v", operation)
	}
}

func TestSandboxHostClientSurfacesResponseCloseFailure(t *testing.T) {
	closeFailure := errors.New("response close failed")
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: &closeErrorBody{
				Reader:   strings.NewReader(`{"operationRef":"operation:1"}`),
				closeErr: closeFailure,
			},
			Header: make(http.Header),
		}, nil
	})}
	client, err := compute.NewSandboxHostClient("http://sandbox-host.internal", "host-secret", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prepare(t.Context(), conformanceComputeRequest()); !errors.Is(err, closeFailure) {
		t.Fatalf("Prepare() error = %v, want response close failure", err)
	}
}

func newSandboxHostServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	state := contracts.InstanceStateReady
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer host-secret" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch request.URL.Path {
		case "/v1/ready":
			writeTestJSON(writer, map[string]bool{"ready": true})
		case "/v1/compute:prepare":
			writeTestJSON(writer, ports.PreparedCompute{OperationRef: "operation:1"})
		case "/v1/compute:start":
			state = contracts.InstanceStateReady
			writeTestJSON(writer, ports.RunningCompute{BackendRef: "backend:1", State: state})
		case "/v1/compute:inspect":
			writeTestJSON(writer, ports.ComputeStatus{State: state})
		case "/v1/compute:execute":
			writeTestJSON(writer, contracts.ExecuteResult{Stdout: "ok", ExitCode: 0})
		case "/v1/compute:stop":
			state = contracts.InstanceStateStopped
			writeTestJSON(writer, struct{}{})
		case "/v1/compute:destroy", "/v1/environment:purge":
			state = contracts.InstanceStateDestroyed
			writeTestJSON(writer, struct{}{})
		case "/v1/compute:checkpoint":
			writeTestJSON(writer, ports.CheckpointResult{
				OpaqueRef:   "snapshot:1",
				ContentHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SizeBytes:   4096,
			})
		case "/v1/workspace:checkpoint":
			writeTestJSON(writer, ports.CheckpointResult{
				OpaqueRef: "snapshot:workspace", ContentHash: strings.Repeat("c", 64), SizeBytes: 4096,
			})
		case "/v1/workspace:materialize":
			writeTestJSON(writer, struct{}{})
		case "/v1/artifacts:exchange":
			writeTestJSON(writer, ports.ArtifactExchangeResult{
				OpaqueRef: "artifact:1", SizeBytes: 42,
				SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			})
		default:
			http.Error(writer, "unknown path", http.StatusNotFound)
		}
	}))
}

func conformanceComputeRequest() ports.ComputeRequest {
	return ports.ComputeRequest{
		Environment: contracts.Environment{
			ContractVersion: contracts.ContractVersionV1,
			ID:              "env",
			Metadata:        map[string]string{},
		},
		Workspace: contracts.Workspace{
			ContractVersion: contracts.ContractVersionV1,
			ID:              "wsp",
		},
		ResourceClass: contracts.ResourceClass{
			ContractVersion: contracts.ContractVersionV1,
			ID:              contracts.ResourceClassAgentStandard,
		},
		Instance: contracts.Instance{
			ContractVersion: contracts.ContractVersionV1,
			ID:              "ins",
			EnvironmentID:   "env",
			Generation:      1,
		},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type closeErrorBody struct {
	io.Reader
	closeErr error
}

func (body *closeErrorBody) Close() error {
	return body.closeErr
}

func writeTestJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		panic(err)
	}
}

// Package compute provides provider-neutral compute adapters.
package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"secondstack/sandbox-service/internal/ports"
	"secondstack/sandbox-service/pkg/contracts"
)

const sandboxHostContractVersion = "sandbox-host.secondstack.ai/v1"
const maxSandboxHostResponseBytes = 1 << 20

// SandboxHostClient is the production ComputeBackend for the privileged Sandbox Host boundary.
type SandboxHostClient struct {
	baseURL *url.URL
	token   string
	client  *http.Client
}

// NewSandboxHostClient constructs a ComputeBackend using stable Sandbox domain requests.
func NewSandboxHostClient(rawURL, token string, client *http.Client) (*SandboxHostClient, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.Path != "" {
		return nil, errors.New("Sandbox Host URL must be an absolute HTTP origin")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("Sandbox Host token is required")
	}
	if client == nil {
		return nil, errors.New("Sandbox Host HTTP client is required")
	}
	return &SandboxHostClient{baseURL: baseURL, token: token, client: client}, nil
}

func (c *SandboxHostClient) Ready(ctx context.Context) error {
	var response struct {
		Ready bool `json:"ready"`
	}
	if err := c.request(ctx, http.MethodGet, "/v1/ready", nil, &response); err != nil {
		return err
	}
	if !response.Ready {
		return errors.New("Sandbox Host readiness response is not ready")
	}
	return nil
}

func (c *SandboxHostClient) Prepare(ctx context.Context, request ports.ComputeRequest) (ports.PreparedCompute, error) {
	var response ports.PreparedCompute
	computeRequest := newHostComputeRequest(request)
	err := c.request(ctx, http.MethodPost, "/v1/compute:prepare", hostEnvelope{ContractVersion: sandboxHostContractVersion, Compute: &computeRequest}, &response)
	return response, err
}

func (c *SandboxHostClient) Start(ctx context.Context, prepared ports.PreparedCompute, request ports.ComputeRequest) (ports.RunningCompute, error) {
	var response ports.RunningCompute
	computeRequest := newHostComputeRequest(request)
	err := c.request(ctx, http.MethodPost, "/v1/compute:start", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Compute: &computeRequest, OperationRef: prepared.OperationRef,
	}, &response)
	return response, err
}

func (c *SandboxHostClient) Inspect(ctx context.Context, identity ports.ComputeIdentity) (ports.ComputeStatus, error) {
	var response ports.ComputeStatus
	err := c.request(ctx, http.MethodPost, "/v1/compute:inspect", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Identity: &identity,
	}, &response)
	return response, err
}

func (c *SandboxHostClient) Stop(ctx context.Context, identity ports.ComputeIdentity) error {
	return c.request(ctx, http.MethodPost, "/v1/compute:stop", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Identity: &identity,
	}, &struct{}{})
}

func (c *SandboxHostClient) Destroy(ctx context.Context, identity ports.ComputeIdentity) error {
	return c.request(ctx, http.MethodPost, "/v1/compute:destroy", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Identity: &identity,
	}, &struct{}{})
}

func (c *SandboxHostClient) Purge(ctx context.Context, environment contracts.Environment, workspace contracts.Workspace) error {
	hostEnvironment := newHostEnvironment(environment)
	hostWorkspace := newHostWorkspace(workspace)
	return c.request(ctx, http.MethodPost, "/v1/environment:purge", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Environment: &hostEnvironment, Workspace: &hostWorkspace,
	}, &struct{}{})
}

func (c *SandboxHostClient) Checkpoint(ctx context.Context, identity ports.ComputeIdentity) (ports.CheckpointResult, error) {
	var response ports.CheckpointResult
	err := c.request(ctx, http.MethodPost, "/v1/compute:checkpoint", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Identity: &identity,
	}, &response)
	return response, err
}

func (c *SandboxHostClient) CheckpointWorkspace(ctx context.Context, environment contracts.Environment, workspace contracts.Workspace) (ports.CheckpointResult, error) {
	var response ports.CheckpointResult
	hostEnvironment := newHostEnvironment(environment)
	hostWorkspace := newHostWorkspace(workspace)
	err := c.request(ctx, http.MethodPost, "/v1/workspace:checkpoint", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Environment: &hostEnvironment, Workspace: &hostWorkspace,
	}, &response)
	return response, err
}

func (c *SandboxHostClient) MaterializeWorkspace(ctx context.Context, environment contracts.Environment, workspace contracts.Workspace, snapshot contracts.Snapshot) error {
	hostEnvironment := newHostEnvironment(environment)
	hostWorkspace := newHostWorkspace(workspace)
	hostSnapshot := newHostSnapshot(snapshot)
	return c.request(ctx, http.MethodPost, "/v1/workspace:materialize", hostEnvelope{
		ContractVersion: sandboxHostContractVersion,
		Environment:     &hostEnvironment, Workspace: &hostWorkspace, Snapshot: &hostSnapshot,
	}, &struct{}{})
}

func (c *SandboxHostClient) ExchangeArtifact(ctx context.Context, input ports.ArtifactExchangeInput) (ports.ArtifactExchangeResult, error) {
	var response ports.ArtifactExchangeResult
	err := c.request(ctx, http.MethodPost, "/v1/artifacts:exchange", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Artifact: &input,
	}, &response)
	return response, err
}

func (c *SandboxHostClient) Execute(ctx context.Context, input ports.ExecuteInput) (contracts.ExecuteResult, error) {
	var response contracts.ExecuteResult
	executeInput := newHostExecuteInput(input)
	err := c.request(ctx, http.MethodPost, "/v1/compute:execute", hostEnvelope{
		ContractVersion: sandboxHostContractVersion, Execute: &executeInput,
	}, &response)
	return response, err
}

func (c *SandboxHostClient) OpenWorkspaceFile(ctx context.Context, input ports.WorkspaceFileInput) (io.ReadCloser, int64, error) {
	endpoint := c.workspaceFileEndpoint(input)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("Sandbox Host transport: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxSandboxHostResponseBytes+1))
		if readErr != nil {
			return nil, 0, errors.Join(readErr, response.Body.Close())
		}
		responseErr := fmt.Errorf("Sandbox Host rejected file read with HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
		return nil, 0, errors.Join(responseErr, response.Body.Close())
	}
	if response.ContentLength < 0 {
		return nil, 0, errors.Join(
			errors.New("Sandbox Host file response requires Content-Length"),
			response.Body.Close(),
		)
	}
	return response.Body, response.ContentLength, nil
}

func (c *SandboxHostClient) PutWorkspaceFile(ctx context.Context, input ports.WorkspaceFileInput, reader io.Reader) (ports.WorkspaceFileWriteResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.workspaceFileEndpoint(input), reader)
	if err != nil {
		return ports.WorkspaceFileWriteResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.client.Do(request)
	if err != nil {
		return ports.WorkspaceFileWriteResult{}, fmt.Errorf("Sandbox Host transport: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxSandboxHostResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return ports.WorkspaceFileWriteResult{}, errors.Join(readErr, closeErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ports.WorkspaceFileWriteResult{}, fmt.Errorf("Sandbox Host rejected file write with HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var result ports.WorkspaceFileWriteResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return ports.WorkspaceFileWriteResult{}, fmt.Errorf("decode Sandbox Host file write response: %w", err)
	}
	return result, nil
}

func (c *SandboxHostClient) workspaceFileEndpoint(input ports.WorkspaceFileInput) string {
	endpoint := *c.baseURL
	endpoint.Path = "/v1/workspace:file"
	query := endpoint.Query()
	query.Set("environmentId", input.Identity.EnvironmentID)
	query.Set("instanceId", input.Identity.InstanceID)
	query.Set("generation", fmt.Sprintf("%d", input.Identity.Generation))
	query.Set("backendRef", input.Identity.BackendRef)
	query.Set("path", input.Path)
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

type hostEnvelope struct {
	ContractVersion string                       `json:"contractVersion"`
	Compute         *hostComputeRequest          `json:"compute,omitempty"`
	Identity        *ports.ComputeIdentity       `json:"identity,omitempty"`
	OperationRef    string                       `json:"operationRef,omitempty"`
	Artifact        *ports.ArtifactExchangeInput `json:"artifact,omitempty"`
	Execute         *hostExecuteInput            `json:"execute,omitempty"`
	Environment     *hostEnvironment             `json:"environment,omitempty"`
	Workspace       *hostWorkspace               `json:"workspace,omitempty"`
	Snapshot        *hostSnapshot                `json:"snapshot,omitempty"`
}

type hostExecuteOperation struct {
	Operation            string            `json:"operation"`
	Command              string            `json:"command,omitempty"`
	Args                 []string          `json:"args,omitempty"`
	Cwd                  string            `json:"cwd,omitempty"`
	Environment          map[string]string `json:"environment,omitempty"`
	TimeoutMillis        int64             `json:"timeoutMillis,omitempty"`
	Path                 string            `json:"path,omitempty"`
	Content              string            `json:"content,omitempty"`
	ContentBase64        string            `json:"contentBase64,omitempty"`
	Encoding             string            `json:"encoding,omitempty"`
	Recursive            bool              `json:"recursive,omitempty"`
	Force                bool              `json:"force,omitempty"`
	AllowedConnectionIDs []string          `json:"allowedConnectionIds"`
}

type hostExecuteInput struct {
	Identity  ports.ComputeIdentity `json:"identity"`
	Operation hostExecuteOperation  `json:"operation"`
}

type hostEnvironment struct {
	ID                string            `json:"id"`
	TenantRef         string            `json:"tenantRef"`
	SubjectRef        string            `json:"subjectRef"`
	EnvironmentKey    string            `json:"environmentKey"`
	WorkspaceID       string            `json:"workspaceId"`
	CurrentGeneration int64             `json:"currentGeneration"`
	Metadata          map[string]string `json:"metadata"`
}

type hostWorkspace struct {
	ID         string `json:"id"`
	StorageRef string `json:"storageRef"`
}

type hostSnapshot struct {
	OpaqueRef   string `json:"opaqueRef"`
	ContentHash string `json:"contentHash"`
	SizeBytes   int64  `json:"sizeBytes"`
}

type hostResourceClass struct {
	CPUMillis    int64 `json:"cpuMillis"`
	MemoryBytes  int64 `json:"memoryBytes"`
	DiskBytes    int64 `json:"diskBytes"`
	ProcessLimit int64 `json:"processLimit"`
}

type hostInstance struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
}

type hostComputeRequest struct {
	Environment   hostEnvironment   `json:"environment"`
	Workspace     hostWorkspace     `json:"workspace"`
	ResourceClass hostResourceClass `json:"resourceClass"`
	Instance      hostInstance      `json:"instance"`
}

func newHostComputeRequest(request ports.ComputeRequest) hostComputeRequest {
	return hostComputeRequest{
		Environment: newHostEnvironment(request.Environment),
		Workspace:   newHostWorkspace(request.Workspace),
		ResourceClass: hostResourceClass{
			CPUMillis:    request.ResourceClass.CPUMillis,
			MemoryBytes:  request.ResourceClass.MemoryBytes,
			DiskBytes:    request.ResourceClass.DiskBytes,
			ProcessLimit: request.ResourceClass.ProcessLimit,
		},
		Instance: hostInstance{
			ID:         request.Instance.ID,
			Generation: request.Instance.Generation,
		},
	}
}

func newHostEnvironment(environment contracts.Environment) hostEnvironment {
	return hostEnvironment{
		ID:                environment.ID,
		TenantRef:         environment.TenantRef,
		SubjectRef:        environment.SubjectRef,
		EnvironmentKey:    environment.EnvironmentKey,
		WorkspaceID:       environment.WorkspaceID,
		CurrentGeneration: environment.CurrentGeneration,
		Metadata:          environment.Metadata,
	}
}

func newHostWorkspace(workspace contracts.Workspace) hostWorkspace {
	return hostWorkspace{
		ID:         workspace.ID,
		StorageRef: workspace.StorageRef,
	}
}

func newHostSnapshot(snapshot contracts.Snapshot) hostSnapshot {
	return hostSnapshot{
		OpaqueRef:   snapshot.OpaqueRef,
		ContentHash: snapshot.ContentHash,
		SizeBytes:   snapshot.SizeBytes,
	}
}

func newHostExecuteInput(input ports.ExecuteInput) hostExecuteInput {
	operation := input.Operation
	return hostExecuteInput{
		Identity: input.Identity,
		Operation: hostExecuteOperation{
			Operation:            operation.Operation,
			Command:              operation.Command,
			Args:                 operation.Args,
			Cwd:                  operation.Cwd,
			Environment:          operation.Environment,
			TimeoutMillis:        operation.TimeoutMillis,
			Path:                 operation.Path,
			Content:              operation.Content,
			ContentBase64:        operation.ContentBase64,
			Encoding:             operation.Encoding,
			Recursive:            operation.Recursive,
			Force:                operation.Force,
			AllowedConnectionIDs: operation.AllowedConnectionIDs,
		},
	}
}

func (c *SandboxHostClient) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Sandbox Host request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := *c.baseURL
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("create Sandbox Host request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("Sandbox Host transport: %w", err)
	}
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maxSandboxHostResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return fmt.Errorf("read Sandbox Host response: %w", errors.Join(readErr, closeErr))
	}
	if len(payload) > maxSandboxHostResponseBytes {
		return errors.New("Sandbox Host response exceeds limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Sandbox Host rejected operation with HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode Sandbox Host response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("Sandbox Host response contains trailing JSON")
	}
	return nil
}

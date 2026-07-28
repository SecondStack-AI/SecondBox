package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
)

type openAPI struct {
	Info struct {
		Version string `json:"version"`
	} `json:"info"`
	Paths      map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]json.RawMessage `json:"schemas"`
	} `json:"components"`
}

func main() {
	check := flag.Bool("check", false, "fail if generated clients differ")
	flag.Parse()
	if err := run(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(check bool) error {
	specBytes, err := os.ReadFile("contracts/sandbox-service.openapi.json")
	if err != nil {
		return err
	}
	var spec openAPI
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return fmt.Errorf("decode Sandbox Service OpenAPI: %w", err)
	}
	if err := validate(spec); err != nil {
		return err
	}
	sum := sha256.Sum256(specBytes)
	sourceHash := hex.EncodeToString(sum[:])
	outputs := map[string][]byte{
		"gen/go/sandboxclient/client.gen.go":   []byte(goClient(sourceHash)),
		"gen/typescript/sandbox-client.gen.ts": []byte(typeScriptClient(sourceHash)),
		"gen/python/sandbox_client_gen.py":     []byte(pythonClient(sourceHash)),
	}
	outputs["gen/go/sandboxclient/client.gen.go"], err = format.Source(outputs["gen/go/sandboxclient/client.gen.go"])
	if err != nil {
		return fmt.Errorf("format generated Go client: %w", err)
	}
	for path, content := range outputs {
		formatted := bytes.TrimSpace(content)
		formatted = append(formatted, '\n')
		if check {
			current, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read generated client %s: %w", path, err)
			}
			if !bytes.Equal(current, formatted) {
				return fmt.Errorf("generated client %s is stale", path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, formatted, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func validate(spec openAPI) error {
	if spec.Info.Version != "sandbox.secondstack.ai/v1" {
		return errors.New("OpenAPI contract version is not sandbox.secondstack.ai/v1")
	}
	requiredSchemas := []string{
		"Environment", "Instance", "Lease", "Workspace", "WorkspaceUsage", "Snapshot", "Artifact",
		"ResourceClass", "LifecyclePolicy", "ExecuteRequest", "ExecuteResult", "WorkspaceVersion",
	}
	for _, name := range requiredSchemas {
		if _, ok := spec.Components.Schemas[name]; !ok {
			return fmt.Errorf("OpenAPI schema %s is required", name)
		}
	}
	requiredOperations := []string{
		"resolveEnvironment", "getEnvironment", "startEnvironment", "inspectEnvironment",
		"stopEnvironment", "executeEnvironment", "checkpointEnvironment", "exchangeArtifact", "acquireLease",
		"renewLease", "releaseLease", "listResourceClasses", "listLifecyclePolicies",
		"purgeEnvironment", "materializeWorkspaceVersion", "commitWorkspaceVersion",
		"getCurrentWorkspaceVersion", "getWorkspaceVersion",
		"openWorkspaceFile", "putWorkspaceFile", "getWorkspaceUsage",
	}
	serialized, err := json.Marshal(spec.Paths)
	if err != nil {
		return err
	}
	for _, operation := range requiredOperations {
		if !bytes.Contains(serialized, []byte(`"operationId":"`+operation+`"`)) {
			return fmt.Errorf("OpenAPI operation %s is required", operation)
		}
	}
	return nil
}

func header(comment, hash string) string {
	return comment + " Code generated from sandbox-service.openapi.json (sha256 " + hash + "); DO NOT EDIT.\n"
}

func goClient(hash string) string {
	return header("//", hash) + `
package sandboxclient

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

	"secondstack/sandbox-service/pkg/contracts"
)

const ContractVersion = contracts.ContractVersionV1

type Client struct {
	baseURL *url.URL
	token string
	httpClient *http.Client
}

func New(rawURL, token string, httpClient *http.Client) (*Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return nil, errors.New("Sandbox Service URL must be absolute")
	}
	if token == "" || httpClient == nil {
		return nil, errors.New("Sandbox Service token and HTTP client are required")
	}
	return &Client{baseURL: baseURL, token: token, httpClient: httpClient}, nil
}

func (c *Client) ResolveEnvironment(ctx context.Context, input contracts.ResolveEnvironmentRequest) (contracts.ResolveEnvironmentResponse, error) {
	var output contracts.ResolveEnvironmentResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments:resolve", input, &output)
	return output, err
}

func (c *Client) GetEnvironment(ctx context.Context, id string) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodGet, "/v1/environments/"+url.PathEscape(id), nil, &output)
	return output, err
}

func (c *Client) GetWorkspaceUsage(ctx context.Context, tenantRef, subjectRef string) (contracts.WorkspaceUsage, error) {
	var output contracts.WorkspaceUsage
	endpoint := "/v1/workspace-usage?tenantRef=" + url.QueryEscape(tenantRef) + "&subjectRef=" + url.QueryEscape(subjectRef)
	err := c.request(ctx, http.MethodGet, endpoint, nil, &output)
	return output, err
}

func (c *Client) StartEnvironment(ctx context.Context, id string, input contracts.EnvironmentGenerationRequest) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":start", input, &output)
	return output, err
}

func (c *Client) InspectEnvironment(ctx context.Context, id string) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":inspect", nil, &output)
	return output, err
}

func (c *Client) StopEnvironment(ctx context.Context, id string, input contracts.EnvironmentGenerationRequest) (contracts.LifecycleResponse, error) {
	var output contracts.LifecycleResponse
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":stop", input, &output)
	return output, err
}

func (c *Client) ExecuteEnvironment(ctx context.Context, id string, input contracts.ExecuteRequest) (contracts.ExecuteResult, error) {
	var output contracts.ExecuteResult
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":execute", input, &output)
	return output, err
}

func (c *Client) PurgeEnvironment(ctx context.Context, id string, input contracts.PurgeEnvironmentRequest) error {
	return c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":purge", input, &struct{}{})
}

func (c *Client) CommitWorkspaceVersion(ctx context.Context, id string, input contracts.CommitWorkspaceVersionRequest) (contracts.WorkspaceVersion, error) {
	var output contracts.WorkspaceVersion
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+"/versions:commit", input, &output)
	return output, err
}

func (c *Client) GetCurrentWorkspaceVersion(ctx context.Context, id string) (contracts.WorkspaceVersion, error) {
	var output contracts.WorkspaceVersion
	err := c.request(ctx, http.MethodGet, "/v1/environments/"+url.PathEscape(id)+"/versions:current", nil, &output)
	return output, err
}

func (c *Client) GetWorkspaceVersion(ctx context.Context, id string, logicalVersion int64) (contracts.WorkspaceVersion, error) {
	var output contracts.WorkspaceVersion
	err := c.request(ctx, http.MethodGet, fmt.Sprintf("/v1/environments/%s/versions/%d", url.PathEscape(id), logicalVersion), nil, &output)
	return output, err
}

func (c *Client) MaterializeWorkspaceVersion(ctx context.Context, targetID string, input contracts.MaterializeWorkspaceVersionRequest) error {
	return c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(targetID)+":materialize", input, &struct{}{})
}

func (c *Client) CheckpointEnvironment(ctx context.Context, id string, input contracts.CheckpointRequest) (contracts.Snapshot, error) {
	var output contracts.Snapshot
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+":checkpoint", input, &output)
	return output, err
}

func (c *Client) ExchangeArtifact(ctx context.Context, id string, input contracts.ExchangeArtifactRequest) (contracts.Artifact, error) {
	var output contracts.Artifact
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+"/artifacts:exchange", input, &output)
	return output, err
}

type WorkspaceFileWriteResult struct {
	SizeBytes int64 ` + "`json:\"sizeBytes\"`" + `
	SHA256 string ` + "`json:\"sha256\"`" + `
}

func (c *Client) OpenWorkspaceFile(ctx context.Context, environmentID string, expectedGeneration int64, leaseID, path string) (io.ReadCloser, int64, error) {
	endpoint := c.workspaceFileEndpoint(environmentID, expectedGeneration, leaseID, path)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil { return nil, 0, err }
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.httpClient.Do(request)
	if err != nil { return nil, 0, err }
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failureErr := decodeFailure(response.Body)
		return nil, 0, errors.Join(failureErr, response.Body.Close())
	}
	return response.Body, response.ContentLength, nil
}

func (c *Client) PutWorkspaceFile(ctx context.Context, environmentID string, expectedGeneration int64, leaseID, path string, body io.Reader) (WorkspaceFileWriteResult, error) {
	var output WorkspaceFileWriteResult
	endpoint := c.workspaceFileEndpoint(environmentID, expectedGeneration, leaseID, path)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil { return output, err }
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.httpClient.Do(request)
	if err != nil { return output, err }
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failureErr := decodeFailure(response.Body)
		return output, errors.Join(failureErr, response.Body.Close())
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&output)
	return output, errors.Join(decodeErr, response.Body.Close())
}

func (c *Client) workspaceFileEndpoint(environmentID string, expectedGeneration int64, leaseID, path string) string {
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: "/v1/environments/"+url.PathEscape(environmentID)+"/files"})
	query := endpoint.Query()
	query.Set("expectedGeneration", fmt.Sprintf("%d", expectedGeneration))
	query.Set("leaseId", leaseID)
	query.Set("path", path)
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func (c *Client) AcquireLease(ctx context.Context, id string, input contracts.AcquireLeaseRequest) (contracts.Lease, error) {
	var output contracts.Lease
	err := c.request(ctx, http.MethodPost, "/v1/environments/"+url.PathEscape(id)+"/leases:acquire", input, &output)
	return output, err
}

func (c *Client) RenewLease(ctx context.Context, id string, input contracts.RenewLeaseRequest) (contracts.Lease, error) {
	var output contracts.Lease
	err := c.request(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+":renew", input, &output)
	return output, err
}

func (c *Client) ReleaseLease(ctx context.Context, id string) (contracts.Lease, error) {
	var output contracts.Lease
	err := c.request(ctx, http.MethodPost, "/v1/leases/"+url.PathEscape(id)+":release", nil, &output)
	return output, err
}

func (c *Client) ListResourceClasses(ctx context.Context) ([]contracts.ResourceClass, error) {
	var output []contracts.ResourceClass
	err := c.request(ctx, http.MethodGet, "/v1/resource-classes", nil, &output)
	return output, err
}

func (c *Client) ListLifecyclePolicies(ctx context.Context) ([]contracts.LifecyclePolicy, error) {
	var output []contracts.LifecyclePolicy
	err := c.request(ctx, http.MethodGet, "/v1/lifecycle-policies", nil, &output)
	return output, err
}

func (c *Client) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil { return err }
		body = bytes.NewReader(encoded)
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil { return err }
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(request)
	if err != nil { return err }
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failureErr := decodeFailure(response.Body)
		return errors.Join(failureErr, response.Body.Close())
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
	return errors.Join(decodeErr, response.Body.Close())
}

func decodeFailure(body io.Reader) error {
	var failure contracts.ErrorResponse
	if err := json.NewDecoder(io.LimitReader(body, 1<<20)).Decode(&failure); err != nil { return err }
	return fmt.Errorf("Sandbox Service %s: %s", failure.Code, failure.Message)
}
`
}

func typeScriptClient(hash string) string {
	return header("//", hash) + `
export const CONTRACT_VERSION = "sandbox.secondstack.ai/v1" as const;
export interface ExposedPort { name: string; port: number; protocol: string; visibility: string; }
export interface Environment { contractVersion: typeof CONTRACT_VERSION; id: string; tenantRef: string; subjectRef: string; environmentKey: string; workspaceId: string; imageRef: string; toolchainRef: string; resourceClassId: string; lifecyclePolicyId: string; desiredState: string; state: string; currentGeneration: number; currentInstanceId?: string; snapshotId?: string; exposedPorts: ExposedPort[]; metadata: Record<string, string>; lastActivityAt: string; createdAt: string; updatedAt: string; }
export interface Instance { contractVersion: typeof CONTRACT_VERSION; id: string; environmentId: string; generation: number; state: string; backendRef?: string; failureCode?: string; preparedAt: string; readyAt?: string; stoppedAt?: string; updatedAt: string; }
export interface Lease { contractVersion: typeof CONTRACT_VERSION; id: string; environmentId: string; generation: number; holderRef: string; state: string; expiresAt: string; createdAt: string; updatedAt: string; }
export interface Workspace { contractVersion: typeof CONTRACT_VERSION; id: string; tenantRef: string; subjectRef: string; storageRef: string; generation: number; retainUntil: string; createdAt: string; updatedAt: string; }
export interface WorkspaceUsage { contractVersion: typeof CONTRACT_VERSION; tenantRef: string; subjectRef: string; environmentCount: number; quotaBytes: number; usageBytes: number; }
export interface Snapshot { contractVersion: typeof CONTRACT_VERSION; id: string; environmentId: string; workspaceId: string; generation: number; parentSnapshotId?: string; opaqueRef: string; contentHash: string; metadata: Record<string, string>; createdAt: string; }
export interface Artifact { contractVersion: typeof CONTRACT_VERSION; id: string; environmentId: string; generation: number; name: string; mimeType: string; sizeBytes: number; sha256: string; opaqueRef: string; metadata: Record<string, string>; createdAt: string; }
export interface ResourceClass { contractVersion: typeof CONTRACT_VERSION; id: string; cpuMillis: number; memoryBytes: number; diskBytes: number; processLimit: number; maxExposedPorts: number; createdAt: string; updatedAt: string; }
export interface LifecyclePolicy { contractVersion: typeof CONTRACT_VERSION; id: string; idleStopAfterSeconds: number; retentionSeconds: number; stopComputeWhenIdle: boolean; retainOnExplicitStop: boolean; keepRunningWithoutWake: boolean; createdAt: string; updatedAt: string; }
export interface ResolveEnvironmentRequest { contractVersion: typeof CONTRACT_VERSION; tenantRef: string; subjectRef: string; environmentKey: string; imageRef: string; toolchainRef: string; resourceClassId: string; lifecyclePolicyId: string; metadata: Record<string, string>; }
export interface EnvironmentGenerationRequest { contractVersion: typeof CONTRACT_VERSION; expectedGeneration?: number; }
export interface AcquireLeaseRequest { contractVersion: typeof CONTRACT_VERSION; holderRef: string; ttlSeconds?: number; }
export interface RenewLeaseRequest { contractVersion: typeof CONTRACT_VERSION; ttlSeconds?: number; }
export interface ExecuteRequest { contractVersion: typeof CONTRACT_VERSION; expectedGeneration: number; leaseId: string; operation: string; allowedConnectionIds: string[]; command?: string; args?: string[]; cwd?: string; environment?: Record<string, string>; timeoutMillis?: number; path?: string; content?: string; contentBase64?: string; encoding?: string; recursive?: boolean; force?: boolean; }
export interface ExecuteResult { instanceId: string; stdout?: string; stderr?: string; exitCode?: number; timedOut?: boolean; content?: string; contentBase64?: string; stat?: Record<string, unknown>; entries?: Record<string, unknown>[]; exists?: boolean; error?: string; }
export interface WorkspaceVersion { contractVersion: typeof CONTRACT_VERSION; environmentId: string; logicalVersion: number; sourceGeneration: number; terminalTurnId: string; terminalStatus: string; workspacePresent: boolean; dirty: boolean; contentHash: string; snapshotId?: string; snapshotLogicalVersion?: number; createdAt: string; }
export interface CommitWorkspaceVersionRequest { contractVersion: typeof CONTRACT_VERSION; expectedGeneration: number; terminalTurnId: string; terminalStatus: string; }
export interface MaterializeWorkspaceVersionRequest { contractVersion: typeof CONTRACT_VERSION; sourceEnvironmentId: string; sourceLogicalVersion: number; expectedTargetGeneration: number; }
export interface PurgeEnvironmentRequest { contractVersion: typeof CONTRACT_VERSION; expectedGeneration: number; }
export interface CheckpointRequest { contractVersion: typeof CONTRACT_VERSION; expectedGeneration: number; metadata: Record<string, string>; }
export interface ExchangeArtifactRequest { contractVersion: typeof CONTRACT_VERSION; expectedGeneration: number; sourceRef: string; name: string; mimeType: string; metadata: Record<string, string>; }
export interface LifecycleResponse { environment: Environment; instance?: Instance; }
export interface WorkspaceFileWriteResult { sizeBytes: number; sha256: string; }

export class SandboxClient {
  constructor(private readonly baseUrl: string, private readonly token: string) {}
  resolveEnvironment(input: ResolveEnvironmentRequest): Promise<{environment: Environment; created: boolean}> { return this.request("POST", "/v1/environments:resolve", input); }
  getEnvironment(id: string): Promise<LifecycleResponse> { return this.request("GET", "/v1/environments/" + encodeURIComponent(id)); }
  getWorkspaceUsage(tenantRef: string, subjectRef: string): Promise<WorkspaceUsage> {
    const query = new URLSearchParams({tenantRef, subjectRef});
    return this.request("GET", "/v1/workspace-usage?" + query.toString());
  }
  startEnvironment(id: string, input: EnvironmentGenerationRequest): Promise<LifecycleResponse> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + ":start", input); }
  inspectEnvironment(id: string): Promise<LifecycleResponse> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + ":inspect"); }
  stopEnvironment(id: string, input: EnvironmentGenerationRequest): Promise<LifecycleResponse> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + ":stop", input); }
  executeEnvironment(id: string, input: ExecuteRequest): Promise<ExecuteResult> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + ":execute", input); }
  purgeEnvironment(id: string, input: PurgeEnvironmentRequest): Promise<Record<string, never>> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + ":purge", input); }
  commitWorkspaceVersion(id: string, input: CommitWorkspaceVersionRequest): Promise<WorkspaceVersion> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + "/versions:commit", input); }
  getCurrentWorkspaceVersion(id: string): Promise<WorkspaceVersion> { return this.request("GET", "/v1/environments/" + encodeURIComponent(id) + "/versions:current"); }
  getWorkspaceVersion(id: string, logicalVersion: number): Promise<WorkspaceVersion> { return this.request("GET", "/v1/environments/" + encodeURIComponent(id) + "/versions/" + logicalVersion); }
  materializeWorkspaceVersion(id: string, input: MaterializeWorkspaceVersionRequest): Promise<Record<string, never>> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + ":materialize", input); }
  checkpointEnvironment(id: string, input: CheckpointRequest): Promise<Snapshot> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + ":checkpoint", input); }
  exchangeArtifact(id: string, input: ExchangeArtifactRequest): Promise<Artifact> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + "/artifacts:exchange", input); }
  async openWorkspaceFile(id: string, expectedGeneration: number, leaseId: string, path: string): Promise<ArrayBuffer> {
    const endpoint = this.workspaceFileUrl(id, expectedGeneration, leaseId, path);
    const response = await fetch(endpoint, {headers: {"Authorization": "Bearer " + this.token}});
    if (!response.ok) throw new Error("Sandbox Service file read failed: " + await response.text());
    return response.arrayBuffer();
  }
  async putWorkspaceFile(id: string, expectedGeneration: number, leaseId: string, path: string, body: BodyInit): Promise<WorkspaceFileWriteResult> {
    const endpoint = this.workspaceFileUrl(id, expectedGeneration, leaseId, path);
    const response = await fetch(endpoint, {method: "PUT", headers: {"Authorization": "Bearer " + this.token, "Content-Type": "application/octet-stream"}, body});
    const payload = await response.json();
    if (!response.ok) throw new Error("Sandbox Service file write failed: " + JSON.stringify(payload));
    return payload as WorkspaceFileWriteResult;
  }
  acquireLease(id: string, input: AcquireLeaseRequest): Promise<Lease> { return this.request("POST", "/v1/environments/" + encodeURIComponent(id) + "/leases:acquire", input); }
  renewLease(id: string, input: RenewLeaseRequest): Promise<Lease> { return this.request("POST", "/v1/leases/" + encodeURIComponent(id) + ":renew", input); }
  releaseLease(id: string): Promise<Lease> { return this.request("POST", "/v1/leases/" + encodeURIComponent(id) + ":release"); }
  listResourceClasses(): Promise<ResourceClass[]> { return this.request("GET", "/v1/resource-classes"); }
  listLifecyclePolicies(): Promise<LifecyclePolicy[]> { return this.request("GET", "/v1/lifecycle-policies"); }
  private workspaceFileUrl(id: string, expectedGeneration: number, leaseId: string, path: string): URL {
    const endpoint = new URL("/v1/environments/" + encodeURIComponent(id) + "/files", this.baseUrl);
    endpoint.searchParams.set("expectedGeneration", String(expectedGeneration));
    endpoint.searchParams.set("leaseId", leaseId);
    endpoint.searchParams.set("path", path);
    return endpoint;
  }
  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const response = await fetch(new URL(path, this.baseUrl), {method, headers: {"Authorization": "Bearer " + this.token, "Content-Type": "application/json"}, body: body === undefined ? undefined : JSON.stringify(body)});
    const payload = await response.json();
    if (!response.ok) throw new Error("Sandbox Service request failed: " + JSON.stringify(payload));
    return payload as T;
  }
}
`
}

func pythonClient(hash string) string {
	return header("#", hash) + `
import json
from typing import Any, NotRequired, TypedDict
from urllib.parse import quote, urlencode
from urllib import request

CONTRACT_VERSION = "sandbox.secondstack.ai/v1"

class Environment(TypedDict):
    contractVersion: str
    id: str
    tenantRef: str
    subjectRef: str
    environmentKey: str
    workspaceId: str
    imageRef: str
    toolchainRef: str
    resourceClassId: str
    lifecyclePolicyId: str
    desiredState: str
    state: str
    currentGeneration: int
    currentInstanceId: NotRequired[str]
    snapshotId: NotRequired[str]
    exposedPorts: list[dict[str, Any]]
    metadata: dict[str, str]
    lastActivityAt: str
    createdAt: str
    updatedAt: str

class Instance(TypedDict):
    contractVersion: str
    id: str
    environmentId: str
    generation: int
    state: str
    backendRef: NotRequired[str]
    failureCode: NotRequired[str]
    preparedAt: str
    readyAt: NotRequired[str]
    stoppedAt: NotRequired[str]
    updatedAt: str

class Lease(TypedDict):
    contractVersion: str
    id: str
    environmentId: str
    generation: int
    holderRef: str
    state: str
    expiresAt: str
    createdAt: str
    updatedAt: str

class Workspace(TypedDict):
    contractVersion: str
    id: str
    tenantRef: str
    subjectRef: str
    storageRef: str
    generation: int
    retainUntil: str
    createdAt: str
    updatedAt: str

class WorkspaceUsage(TypedDict):
    contractVersion: str
    tenantRef: str
    subjectRef: str
    environmentCount: int
    quotaBytes: int
    usageBytes: int

class Snapshot(TypedDict):
    contractVersion: str
    id: str
    environmentId: str
    workspaceId: str
    generation: int
    parentSnapshotId: NotRequired[str]
    opaqueRef: str
    contentHash: str
    metadata: dict[str, str]
    createdAt: str

class Artifact(TypedDict):
    contractVersion: str
    id: str
    environmentId: str
    generation: int
    name: str
    mimeType: str
    sizeBytes: int
    sha256: str
    opaqueRef: str
    metadata: dict[str, str]
    createdAt: str

class ResourceClass(TypedDict):
    contractVersion: str
    id: str
    cpuMillis: int
    memoryBytes: int
    diskBytes: int
    processLimit: int
    maxExposedPorts: int
    createdAt: str
    updatedAt: str

class LifecyclePolicy(TypedDict):
    contractVersion: str
    id: str
    idleStopAfterSeconds: int
    retentionSeconds: int
    stopComputeWhenIdle: bool
    retainOnExplicitStop: bool
    keepRunningWithoutWake: bool
    createdAt: str
    updatedAt: str

class ExecuteRequest(TypedDict):
    contractVersion: str
    expectedGeneration: int
    leaseId: str
    operation: str
    allowedConnectionIds: list[str]
    command: NotRequired[str]
    args: NotRequired[list[str]]
    cwd: NotRequired[str]
    environment: NotRequired[dict[str, str]]
    timeoutMillis: NotRequired[int]
    path: NotRequired[str]
    content: NotRequired[str]
    contentBase64: NotRequired[str]
    encoding: NotRequired[str]
    recursive: NotRequired[bool]
    force: NotRequired[bool]

class ExecuteResult(TypedDict):
    instanceId: str
    stdout: NotRequired[str]
    stderr: NotRequired[str]
    exitCode: NotRequired[int]
    timedOut: NotRequired[bool]
    content: NotRequired[str]
    contentBase64: NotRequired[str]
    stat: NotRequired[dict[str, Any]]
    entries: NotRequired[list[dict[str, Any]]]
    exists: NotRequired[bool]
    error: NotRequired[str]

class WorkspaceVersion(TypedDict):
    contractVersion: str
    environmentId: str
    logicalVersion: int
    sourceGeneration: int
    terminalTurnId: str
    terminalStatus: str
    workspacePresent: bool
    dirty: bool
    contentHash: str
    snapshotId: NotRequired[str]
    snapshotLogicalVersion: NotRequired[int]
    createdAt: str

class WorkspaceFileWriteResult(TypedDict):
    sizeBytes: int
    sha256: str

class SandboxClient:
    def __init__(self, base_url: str, token: str) -> None:
        self._base_url = base_url.rstrip("/")
        self._token = token

    def resolve_environment(self, value: dict[str, Any]) -> dict[str, Any]:
        return self._request("POST", "/v1/environments:resolve", value)

    def get_environment(self, environment_id: str) -> dict[str, Any]:
        return self._request("GET", f"/v1/environments/{quote(environment_id, safe='')}", None)

    def get_workspace_usage(self, tenant_ref: str, subject_ref: str) -> WorkspaceUsage:
        query = urlencode({"tenantRef": tenant_ref, "subjectRef": subject_ref})
        return self._request("GET", f"/v1/workspace-usage?{query}", None)

    def start_environment(self, environment_id: str, generation: int) -> dict[str, Any]:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}:start", {"contractVersion": CONTRACT_VERSION, "expectedGeneration": generation})

    def inspect_environment(self, environment_id: str) -> dict[str, Any]:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}:inspect", None)

    def stop_environment(self, environment_id: str, generation: int) -> dict[str, Any]:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}:stop", {"contractVersion": CONTRACT_VERSION, "expectedGeneration": generation})

    def execute_environment(self, environment_id: str, value: ExecuteRequest) -> ExecuteResult:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}:execute", value)

    def purge_environment(self, environment_id: str, value: dict[str, Any]) -> dict[str, Any]:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}:purge", value)

    def commit_workspace_version(self, environment_id: str, value: dict[str, Any]) -> WorkspaceVersion:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}/versions:commit", value)

    def get_current_workspace_version(self, environment_id: str) -> WorkspaceVersion:
        return self._request("GET", f"/v1/environments/{quote(environment_id, safe='')}/versions:current", None)

    def get_workspace_version(self, environment_id: str, logical_version: int) -> WorkspaceVersion:
        return self._request("GET", f"/v1/environments/{quote(environment_id, safe='')}/versions/{logical_version}", None)

    def materialize_workspace_version(self, environment_id: str, value: dict[str, Any]) -> dict[str, Any]:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}:materialize", value)

    def checkpoint_environment(self, environment_id: str, value: dict[str, Any]) -> Snapshot:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}:checkpoint", value)

    def exchange_artifact(self, environment_id: str, value: dict[str, Any]) -> Artifact:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}/artifacts:exchange", value)

    def open_workspace_file(self, environment_id: str, expected_generation: int, lease_id: str, path: str) -> bytes:
        endpoint = self._workspace_file_path(environment_id, expected_generation, lease_id, path)
        call = request.Request(self._base_url + endpoint, method="GET", headers={"Authorization": "Bearer " + self._token})
        with request.urlopen(call) as response:
            return response.read()

    def put_workspace_file(self, environment_id: str, expected_generation: int, lease_id: str, path: str, body: bytes) -> WorkspaceFileWriteResult:
        endpoint = self._workspace_file_path(environment_id, expected_generation, lease_id, path)
        call = request.Request(self._base_url + endpoint, data=body, method="PUT", headers={"Authorization": "Bearer " + self._token, "Content-Type": "application/octet-stream"})
        with request.urlopen(call) as response:
            return json.load(response)

    def acquire_lease(self, environment_id: str, value: dict[str, Any]) -> Lease:
        return self._request("POST", f"/v1/environments/{quote(environment_id, safe='')}/leases:acquire", value)

    def renew_lease(self, lease_id: str, value: dict[str, Any]) -> Lease:
        return self._request("POST", f"/v1/leases/{quote(lease_id, safe='')}:renew", value)

    def release_lease(self, lease_id: str) -> Lease:
        return self._request("POST", f"/v1/leases/{quote(lease_id, safe='')}:release", None)

    def list_resource_classes(self) -> list[ResourceClass]:
        return self._request("GET", "/v1/resource-classes", None)

    def list_lifecycle_policies(self) -> list[LifecyclePolicy]:
        return self._request("GET", "/v1/lifecycle-policies", None)

    def _workspace_file_path(self, environment_id: str, expected_generation: int, lease_id: str, path: str) -> str:
        query = urlencode({"expectedGeneration": expected_generation, "leaseId": lease_id, "path": path})
        return f"/v1/environments/{quote(environment_id, safe='')}/files?{query}"

    def _request(self, method: str, path: str, body: dict[str, Any] | None) -> dict[str, Any]:
        payload = None if body is None else json.dumps(body).encode()
        call = request.Request(self._base_url + path, data=payload, method=method, headers={"Authorization": "Bearer " + self._token, "Content-Type": "application/json"})
        with request.urlopen(call) as response:
            return json.load(response)
`
}

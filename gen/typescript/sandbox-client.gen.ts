// Code generated from sandbox-service.openapi.json (sha256 78d7f7132f93eb4e415a11edb04184a05ac0b4952d8d97e582291c6d079ff679); DO NOT EDIT.

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

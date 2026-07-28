// Code generated from contracts/openapi/v1/secondbox.openapi.json (sha256 90ee902600bd204bb4fbbd22dda45b9b887fdd2fba3d7123172c0d1d968f9bf4); DO NOT EDIT.

/** OpenAPI info.version represented by this generated transport. */
export const CONTRACT_VERSION = "1.0.0";

/** is the wire representation of the api key schema. */
export interface APIKey {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** expiresAt is the canonical JSON field. */
  expiresAt?: Timestamp;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** lastUsedAt is the canonical JSON field. */
  lastUsedAt?: Timestamp;
  /** name is the canonical JSON field. */
  name: string;
  /** prefix is the canonical JSON field. */
  prefix: string;
  /** revision is the canonical JSON field. */
  revision: number;
  /** revokedAt is the canonical JSON field. */
  revokedAt?: Timestamp;
  /** scopes is the canonical JSON field. */
  scopes: readonly ServiceAccountScope[];
  /** serviceAccountId is the canonical JSON field. */
  serviceAccountId: OpaqueID;
  /** state is the canonical JSON field. */
  state: APIKeyState;
}

/** is the wire representation of the api key page schema. */
export interface APIKeyPage {
  /** items is the canonical JSON field. */
  items: readonly APIKey[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the api key state schema. */
export type APIKeyState = "active" | "revoked" | "expired";

/** is the wire representation of the acquire lease request schema. */
export interface AcquireLeaseRequest {
  /** durationSeconds is the canonical JSON field. */
  durationSeconds: number;
}

/** is the wire representation of the argv command schema. */
export interface ArgvCommand {
  /** arguments is the canonical JSON field. */
  arguments: readonly string[];
  /** executable is the canonical JSON field. */
  executable: string;
  /** mode is the canonical JSON field. */
  mode: "argv";
}

/** is the wire representation of the artifact schema. */
export interface Artifact {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** expiresAt is the canonical JSON field. */
  expiresAt: Timestamp;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** mediaType is the canonical JSON field. */
  mediaType: string;
  /** metadata is the canonical JSON field. */
  metadata: Metadata;
  /** name is the canonical JSON field. */
  name: string;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** sha256 is the canonical JSON field. */
  sha256: string;
  /** sizeBytes is the canonical JSON field. */
  sizeBytes: number;
}

/** is the wire representation of the artifact page schema. */
export interface ArtifactPage {
  /** items is the canonical JSON field. */
  items: readonly Artifact[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the buffered exec request schema. */
export interface BufferedExecRequest {
  /** command is the canonical JSON field. */
  command: Command;
  /** cwd is the canonical JSON field. */
  cwd?: WorkspacePath;
  /** deadlineMilliseconds is the canonical JSON field. */
  deadlineMilliseconds: number;
  /** environment is the canonical JSON field. */
  environment: StringMap;
  /** maximumOutputBytes is the canonical JSON field. */
  maximumOutputBytes: number;
  /** stdinBase64 is the canonical JSON field. */
  stdinBase64?: string;
}

/** is the wire representation of the checkpoint policy schema. */
export interface CheckpointPolicy {
  /** artifactRetentionSeconds is the canonical JSON field. */
  artifactRetentionSeconds: number;
  /** onStop is the canonical JSON field. */
  onStop: boolean;
  /** retentionSeconds is the canonical JSON field. */
  retentionSeconds: number;
  /** snapshotLimit is the canonical JSON field. */
  snapshotLimit: number;
}

/** is the wire representation of the checkpoint sandbox request schema. */
export interface CheckpointSandboxRequest {
  /** metadata is the canonical JSON field. */
  metadata: Metadata;
}

/** is the wire representation of the command schema. */
export type Command = ShellCommand | ArgvCommand;

/** is the wire representation of the correlation id schema. */
export type CorrelationID = string;

/** is the wire representation of the create api key request schema. */
export interface CreateAPIKeyRequest {
  /** expiresAt is the canonical JSON field. */
  expiresAt?: Timestamp;
  /** name is the canonical JSON field. */
  name: string;
  /** scopes is the canonical JSON field. */
  scopes: readonly ServiceAccountScope[];
}

/** is the wire representation of the create api key response schema. */
export interface CreateAPIKeyResponse {
  /** apiKey is the canonical JSON field. */
  apiKey: APIKey;
  /** credential is the canonical JSON field. */
  credential: string;
}

/** is the wire representation of the create directory request schema. */
export interface CreateDirectoryRequest {
  /** path is the canonical JSON field. */
  path: WorkspacePath;
  /** recursive is the canonical JSON field. */
  recursive: boolean;
}

/** is the wire representation of the create port session request schema. */
export interface CreatePortSessionRequest {
  /** durationSeconds is the canonical JSON field. */
  durationSeconds: number;
  /** name is the canonical JSON field. */
  name: string;
}

/** is the wire representation of the create profile request schema. */
export interface CreateProfileRequest {
  /** name is the canonical JSON field. */
  name: ProfileName;
  /** spec is the canonical JSON field. */
  spec: ProfileRevisionSpec;
}

/** is the wire representation of the create project request schema. */
export interface CreateProjectRequest {
  /** name is the canonical JSON field. */
  name: string;
}

/** is the wire representation of the create runner pool request schema. */
export interface CreateRunnerPoolRequest {
  /** architectures is the canonical JSON field. */
  architectures: RunnerArchitectureList;
  /** capabilities is the canonical JSON field. */
  capabilities: RunnerCapabilityList;
  /** capacityPolicy is the canonical JSON field. */
  capacityPolicy: RunnerCapacityPolicy;
  /** name is the canonical JSON field. */
  name: ProfileName;
  /** state is the canonical JSON field. */
  state: RunnerPoolState;
}

/** is the wire representation of the create sandbox request schema. */
export interface CreateSandboxRequest {
  /** metadata is the canonical JSON field. */
  metadata: Metadata;
  /** profile is the canonical JSON field. */
  profile: ProfileName;
}

/** is the wire representation of the create service account request schema. */
export interface CreateServiceAccountRequest {
  /** name is the canonical JSON field. */
  name: string;
  /** profileGrants is the canonical JSON field. */
  profileGrants: readonly ProfileName[];
  /** scopes is the canonical JSON field. */
  scopes: readonly ServiceAccountScope[];
}

/** is the wire representation of the create snapshot request schema. */
export interface CreateSnapshotRequest {
  /** metadata is the canonical JSON field. */
  metadata: Metadata;
  /** name is the canonical JSON field. */
  name: string;
}

/** is the wire representation of the create terminal request schema. */
export interface CreateTerminalRequest {
  /** columns is the canonical JSON field. */
  columns: number;
  /** command is the canonical JSON field. */
  command: Command;
  /** cwd is the canonical JSON field. */
  cwd?: WorkspacePath;
  /** deadlineMilliseconds is the canonical JSON field. */
  deadlineMilliseconds: number;
  /** detachable is the canonical JSON field. */
  detachable: boolean;
  /** environment is the canonical JSON field. */
  environment: StringMap;
  /** rows is the canonical JSON field. */
  rows: number;
}

/** is the wire representation of the directory listing schema. */
export interface DirectoryListing {
  /** entries is the canonical JSON field. */
  entries: readonly FileStat[];
  /** path is the canonical JSON field. */
  path: WorkspacePath;
}

/** is the wire representation of the exec cancelled schema. */
export interface ExecCancelled {
  /** kind is the canonical JSON field. */
  kind: "cancelled";
  /** output is the canonical JSON field. */
  output: ExecOutput;
}

/** is the wire representation of the exec deadline exceeded schema. */
export interface ExecDeadlineExceeded {
  /** elapsedMilliseconds is the canonical JSON field. */
  elapsedMilliseconds: number;
  /** kind is the canonical JSON field. */
  kind: "deadline_exceeded";
  /** output is the canonical JSON field. */
  output: ExecOutput;
}

/** is the wire representation of the exec exited schema. */
export interface ExecExited {
  /** exitCode is the canonical JSON field. */
  exitCode: number;
  /** kind is the canonical JSON field. */
  kind: "exited";
  /** output is the canonical JSON field. */
  output: ExecOutput;
  /** signal is the canonical JSON field. */
  signal?: number;
}

/** is the wire representation of the exec infrastructure failed schema. */
export interface ExecInfrastructureFailed {
  /** kind is the canonical JSON field. */
  kind: "infrastructure_failed";
  /** message is the canonical JSON field. */
  message: string;
  /** reason is the canonical JSON field. */
  reason: InfrastructureFailureKind;
  /** retryable is the canonical JSON field. */
  retryable: boolean;
}

/** is the wire representation of the exec outcome schema. */
export type ExecOutcome = ExecExited | ExecSpawnFailed | ExecDeadlineExceeded | ExecCancelled | ExecOutputExhausted | ExecInfrastructureFailed;

/** is the wire representation of the exec output schema. */
export interface ExecOutput {
  /** stderrBase64 is the canonical JSON field. */
  stderrBase64: string;
  /** stdoutBase64 is the canonical JSON field. */
  stdoutBase64: string;
}

/** is the wire representation of the exec output exhausted schema. */
export interface ExecOutputExhausted {
  /** kind is the canonical JSON field. */
  kind: "output_exhausted";
  /** limitBytes is the canonical JSON field. */
  limitBytes: number;
  /** output is the canonical JSON field. */
  output: ExecOutput;
}

/** is the wire representation of the exec spawn failed schema. */
export interface ExecSpawnFailed {
  /** kind is the canonical JSON field. */
  kind: "spawn_failed";
  /** message is the canonical JSON field. */
  message: string;
  /** reason is the canonical JSON field. */
  reason: SpawnFailureKind;
}

/** is the wire representation of the exec stream frame schema. */
export type ExecStreamFrame = StreamInputFrame | StreamOutputFrame | StreamCreditFrame | StreamSignalFrame | StreamCancelFrame | StreamOutcomeFrame;

/** is the wire representation of the exec stream session schema. */
export interface ExecStreamSession {
  /** expiresAt is the canonical JSON field. */
  expiresAt: Timestamp;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** state is the canonical JSON field. */
  state: SessionState;
  /** subprotocol is the canonical JSON field. */
  subprotocol: "secondbox.exec.v1";
  /** websocketUrl is the canonical JSON field. */
  websocketUrl: string;
}

/** is the wire representation of the execution policy schema. */
export interface ExecutionPolicy {
  /** maximumBufferedOutputBytes is the canonical JSON field. */
  maximumBufferedOutputBytes: number;
  /** maximumDeadlineMilliseconds is the canonical JSON field. */
  maximumDeadlineMilliseconds: number;
  /** maximumTransferBytes is the canonical JSON field. */
  maximumTransferBytes: number;
  /** streamWindowBytes is the canonical JSON field. */
  streamWindowBytes: number;
  /** terminalDetachSeconds is the canonical JSON field. */
  terminalDetachSeconds: number;
}

/** is the wire representation of the file exists result schema. */
export interface FileExistsResult {
  /** exists is the canonical JSON field. */
  exists: boolean;
  /** path is the canonical JSON field. */
  path: WorkspacePath;
}

/** is the wire representation of the file kind schema. */
export type FileKind = "file" | "directory" | "symbolic_link";

/** is the wire representation of the file stat schema. */
export interface FileStat {
  /** kind is the canonical JSON field. */
  kind: FileKind;
  /** modifiedAt is the canonical JSON field. */
  modifiedAt: Timestamp;
  /** path is the canonical JSON field. */
  path: WorkspacePath;
  /** sizeBytes is the canonical JSON field. */
  sizeBytes: number;
}

/** is the wire representation of the file write result schema. */
export interface FileWriteResult {
  /** path is the canonical JSON field. */
  path: WorkspacePath;
  /** sha256 is the canonical JSON field. */
  sha256: string;
  /** sizeBytes is the canonical JSON field. */
  sizeBytes: number;
}

/** is the wire representation of the infrastructure failure kind schema. */
export type InfrastructureFailureKind = "transport" | "admission" | "generation_fenced" | "lease_fenced" | "guest_agent" | "execution_node" | "service";

/** is the wire representation of the instance schema. */
export interface Instance {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** readyAt is the canonical JSON field. */
  readyAt?: Timestamp;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** state is the canonical JSON field. */
  state: InstanceState;
  /** stoppedAt is the canonical JSON field. */
  stoppedAt?: Timestamp;
  /** terminationReason is the canonical JSON field. */
  terminationReason?: InstanceTerminationReason;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the instance state schema. */
export type InstanceState = "starting" | "ready" | "draining" | "stopping" | "stopped" | "lost" | "failed";

/** is the wire representation of the instance termination reason schema. */
export type InstanceTerminationReason = "requested_drain" | "requested_stop" | "idle_timeout" | "maximum_duration" | "guest_shutdown" | "resource_exhaustion" | "guest_agent_lost" | "runner_lost" | "startup_failed" | "fenced" | "internal_failure";

/** is the wire representation of the lease schema. */
export interface Lease {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** expiresAt is the canonical JSON field. */
  expiresAt: Timestamp;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** state is the canonical JSON field. */
  state: LeaseState;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the lease state schema. */
export type LeaseState = "active" | "released" | "expired" | "fenced";

/** is the wire representation of the lifecycle policy schema. */
export interface LifecyclePolicy {
  /** drainGraceSeconds is the canonical JSON field. */
  drainGraceSeconds: number;
  /** idleSeconds is the canonical JSON field. */
  idleSeconds: number;
  /** initialState is the canonical JSON field. */
  initialState: string;
  /** leaseSeconds is the canonical JSON field. */
  leaseSeconds: number;
  /** maximumDurationSeconds is the canonical JSON field. */
  maximumDurationSeconds: number;
}

/** is the wire representation of the metadata schema. */
export type Metadata = Readonly<Record<string, string>>;

/** is the wire representation of the network destination schema. */
export interface NetworkDestination {
  /** cidr is the canonical JSON field. */
  cidr?: string;
  /** domain is the canonical JSON field. */
  domain?: string;
  /** port is the canonical JSON field. */
  port: number;
  /** protocol is the canonical JSON field. */
  protocol: string;
}

/** is the wire representation of the network policy schema. */
export interface NetworkPolicy {
  /** destinations is the canonical JSON field. */
  destinations: readonly NetworkDestination[];
  /** mode is the canonical JSON field. */
  mode: string;
}

/** is the wire representation of the opaque id schema. */
export type OpaqueID = string;

/** is the wire representation of the operation schema. */
export interface Operation {
  /** completedAt is the canonical JSON field. */
  completedAt?: Timestamp;
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** error is the canonical JSON field. */
  error?: Problem;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** kind is the canonical JSON field. */
  kind: OperationKind;
  /** requestId is the canonical JSON field. */
  requestId: CorrelationID;
  /** sandbox is the canonical JSON field. */
  sandbox?: Sandbox;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** startedAt is the canonical JSON field. */
  startedAt?: Timestamp;
  /** state is the canonical JSON field. */
  state: OperationState;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the operation kind schema. */
export type OperationKind = "create" | "start" | "drain" | "stop" | "checkpoint" | "delete" | "cancel_exec" | "cancel_terminal";

/** is the wire representation of the operation state schema. */
export type OperationState = "pending" | "running" | "succeeded" | "failed" | "cancelled";

/** is the wire representation of the ping result schema. */
export interface PingResult {
  /** generation is the canonical JSON field. */
  generation: number;
  /** healthy is the canonical JSON field. */
  healthy: boolean;
  /** observedAt is the canonical JSON field. */
  observedAt: Timestamp;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
}

/** is the wire representation of the port policy schema. */
export interface PortPolicy {
  /** maximumSessionSeconds is the canonical JSON field. */
  maximumSessionSeconds: number;
  /** maximumSessions is the canonical JSON field. */
  maximumSessions: number;
  /** name is the canonical JSON field. */
  name: string;
  /** port is the canonical JSON field. */
  port: number;
  /** protocol is the canonical JSON field. */
  protocol: string;
}

/** is the wire representation of the port session schema. */
export interface PortSession {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** endpoint is the canonical JSON field. */
  endpoint: string;
  /** expiresAt is the canonical JSON field. */
  expiresAt: Timestamp;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** name is the canonical JSON field. */
  name: string;
  /** protocol is the canonical JSON field. */
  protocol: string;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** state is the canonical JSON field. */
  state: string;
}

/** is the wire representation of the problem schema. */
export interface Problem {
  /** code is the canonical JSON field. */
  code: ProblemCode;
  /** details is the canonical JSON field. */
  details?: readonly ProblemDetail[];
  /** requestId is the canonical JSON field. */
  requestId: CorrelationID;
  /** retryable is the canonical JSON field. */
  retryable: boolean;
  /** status is the canonical JSON field. */
  status: number;
  /** title is the canonical JSON field. */
  title: string;
  /** type is the canonical JSON field. */
  type: string;
}

/** is the wire representation of the problem code schema. */
export type ProblemCode = "invalid_request" | "authentication_failed" | "authorization_failed" | "not_found" | "idempotency_conflict" | "precondition_failed" | "state_conflict" | "generation_fenced" | "lease_fenced" | "profile_unavailable" | "quota_exceeded" | "limit_exceeded" | "guest_unavailable" | "execution_node_unavailable" | "dependency_unavailable" | "internal_error" | "wait_expired";

/** is the wire representation of the problem detail schema. */
export interface ProblemDetail {
  /** field is the canonical JSON field. */
  field: string;
  /** reason is the canonical JSON field. */
  reason: string;
}

/** is the wire representation of the profile schema. */
export interface Profile {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** currentRevision is the canonical JSON field. */
  currentRevision: ProfileRevision;
  /** name is the canonical JSON field. */
  name: ProfileName;
  /** revision is the canonical JSON field. */
  revision: number;
  /** state is the canonical JSON field. */
  state: ProfileState;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the profile name schema. */
export type ProfileName = string;

/** is the wire representation of the profile page schema. */
export interface ProfilePage {
  /** items is the canonical JSON field. */
  items: readonly Profile[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the profile revision schema. */
export interface ProfileRevision {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** number is the canonical JSON field. */
  number: number;
  /** spec is the canonical JSON field. */
  spec: ProfileRevisionSpec;
}

/** is the wire representation of the profile revision spec schema. */
export interface ProfileRevisionSpec {
  /** architecture is the canonical JSON field. */
  architecture: string;
  /** backend is the canonical JSON field. */
  backend: string;
  /** checkpoint is the canonical JSON field. */
  checkpoint: CheckpointPolicy;
  /** execution is the canonical JSON field. */
  execution: ExecutionPolicy;
  /** lifecycle is the canonical JSON field. */
  lifecycle: LifecyclePolicy;
  /** network is the canonical JSON field. */
  network: NetworkPolicy;
  /** pool is the canonical JSON field. */
  pool: string;
  /** ports is the canonical JSON field. */
  ports: readonly PortPolicy[];
  /** resources is the canonical JSON field. */
  resources: ResourcePolicy;
  /** runtimeBundleDigest is the canonical JSON field. */
  runtimeBundleDigest: string;
  /** toolchainBundleDigest is the canonical JSON field. */
  toolchainBundleDigest: string;
}

/** is the wire representation of the profile state schema. */
export type ProfileState = "enabled" | "disabled";

/** is the wire representation of the project schema. */
export interface Project {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** name is the canonical JSON field. */
  name: string;
  /** revision is the canonical JSON field. */
  revision: number;
  /** state is the canonical JSON field. */
  state: ProjectState;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the project page schema. */
export interface ProjectPage {
  /** items is the canonical JSON field. */
  items: readonly Project[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the project state schema. */
export type ProjectState = "active" | "disabled";

/** is the wire representation of the remove path request schema. */
export interface RemovePathRequest {
  /** force is the canonical JSON field. */
  force: boolean;
  /** path is the canonical JSON field. */
  path: WorkspacePath;
  /** recursive is the canonical JSON field. */
  recursive: boolean;
}

/** is the wire representation of the renew lease request schema. */
export interface RenewLeaseRequest {
  /** durationSeconds is the canonical JSON field. */
  durationSeconds: number;
}

/** is the wire representation of the resource policy schema. */
export interface ResourcePolicy {
  /** concurrentOperations is the canonical JSON field. */
  concurrentOperations: number;
  /** cpuMillis is the canonical JSON field. */
  cpuMillis: number;
  /** memoryBytes is the canonical JSON field. */
  memoryBytes: number;
  /** processLimit is the canonical JSON field. */
  processLimit: number;
  /** workspaceBytes is the canonical JSON field. */
  workspaceBytes: number;
}

/** is the wire representation of the revise profile request schema. */
export interface ReviseProfileRequest {
  /** spec is the canonical JSON field. */
  spec: ProfileRevisionSpec;
}

/** is the wire representation of the runner schema. */
export interface Runner {
  /** architectures is the canonical JSON field. */
  architectures: readonly string[];
  /** capabilities is the canonical JSON field. */
  capabilities: readonly string[];
  /** capacity is the canonical JSON field. */
  capacity: Readonly<Record<string, number>>;
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** credentialState is the canonical JSON field. */
  credentialState: string;
  /** id is the canonical JSON field. */
  id: RunnerID;
  /** lastSeenAt is the canonical JSON field. */
  lastSeenAt?: Timestamp;
  /** name is the canonical JSON field. */
  name: string;
  /** poolName is the canonical JSON field. */
  poolName: ProfileName;
  /** protocolVersions is the canonical JSON field. */
  protocolVersions: readonly string[];
  /** revision is the canonical JSON field. */
  revision: number;
  /** state is the canonical JSON field. */
  state: string;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the runner architecture list schema. */
export type RunnerArchitectureList = readonly string[];

/** is the wire representation of the runner capability list schema. */
export type RunnerCapabilityList = readonly string[];

/** is the wire representation of the runner capacity policy schema. */
export type RunnerCapacityPolicy = Readonly<Record<string, number>>;

/** is the wire representation of the runner id schema. */
export type RunnerID = string;

/** is the wire representation of the runner page schema. */
export interface RunnerPage {
  /** items is the canonical JSON field. */
  items: readonly Runner[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the runner pool schema. */
export interface RunnerPool {
  /** architectures is the canonical JSON field. */
  architectures: RunnerArchitectureList;
  /** capabilities is the canonical JSON field. */
  capabilities: RunnerCapabilityList;
  /** capacityPolicy is the canonical JSON field. */
  capacityPolicy: RunnerCapacityPolicy;
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** name is the canonical JSON field. */
  name: ProfileName;
  /** readyRunnerCount is the canonical JSON field. */
  readyRunnerCount: number;
  /** revision is the canonical JSON field. */
  revision: number;
  /** state is the canonical JSON field. */
  state: RunnerPoolState;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the runner pool page schema. */
export interface RunnerPoolPage {
  /** items is the canonical JSON field. */
  items: readonly RunnerPool[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the runner pool state schema. */
export type RunnerPoolState = "ready" | "draining" | "offline";

/** is the wire representation of the sandbox schema. */
export interface Sandbox {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** deletedAt is the canonical JSON field. */
  deletedAt?: Timestamp;
  /** desiredState is the canonical JSON field. */
  desiredState: SandboxDesiredState;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** instance is the canonical JSON field. */
  instance?: Instance;
  /** lastActivityAt is the canonical JSON field. */
  lastActivityAt?: Timestamp;
  /** metadata is the canonical JSON field. */
  metadata: Metadata;
  /** profile is the canonical JSON field. */
  profile: ProfileName;
  /** profileRevisionId is the canonical JSON field. */
  profileRevisionId: OpaqueID;
  /** projectId is the canonical JSON field. */
  projectId: OpaqueID;
  /** revision is the canonical JSON field. */
  revision: number;
  /** state is the canonical JSON field. */
  state: SandboxState;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
  /** workspace is the canonical JSON field. */
  workspace: Workspace;
}

/** is the wire representation of the sandbox desired state schema. */
export type SandboxDesiredState = "running" | "stopped" | "deleted";

/** is the wire representation of the sandbox inspection schema. */
export interface SandboxInspection {
  /** activeSessions is the canonical JSON field. */
  activeSessions: number;
  /** generation is the canonical JSON field. */
  generation: number;
  /** guestHealthy is the canonical JSON field. */
  guestHealthy: boolean;
  /** observedAt is the canonical JSON field. */
  observedAt: Timestamp;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
}

/** is the wire representation of the sandbox page schema. */
export interface SandboxPage {
  /** items is the canonical JSON field. */
  items: readonly Sandbox[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the sandbox state schema. */
export type SandboxState = "creating" | "stopped" | "starting" | "ready" | "draining" | "stopping" | "checkpointing" | "failed" | "deleting" | "deleted";

/** is the wire representation of the service account schema. */
export interface ServiceAccount {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** name is the canonical JSON field. */
  name: string;
  /** profileGrants is the canonical JSON field. */
  profileGrants: readonly ProfileName[];
  /** projectId is the canonical JSON field. */
  projectId: OpaqueID;
  /** revision is the canonical JSON field. */
  revision: number;
  /** scopes is the canonical JSON field. */
  scopes: readonly ServiceAccountScope[];
  /** state is the canonical JSON field. */
  state: ServiceAccountState;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the service account page schema. */
export interface ServiceAccountPage {
  /** items is the canonical JSON field. */
  items: readonly ServiceAccount[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the service account scope schema. */
export type ServiceAccountScope = "sandbox:read" | "sandbox:lifecycle" | "sandbox:exec" | "sandbox:files" | "sandbox:artifacts" | "sandbox:ports";

/** is the wire representation of the service account state schema. */
export type ServiceAccountState = "active" | "disabled";

/** is the wire representation of the session state schema. */
export type SessionState = "open" | "detached" | "closing" | "closed";

/** is the wire representation of the shell command schema. */
export interface ShellCommand {
  /** command is the canonical JSON field. */
  command: string;
  /** mode is the canonical JSON field. */
  mode: "shell";
}

/** is the wire representation of the snapshot schema. */
export interface Snapshot {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** expiresAt is the canonical JSON field. */
  expiresAt: Timestamp;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** metadata is the canonical JSON field. */
  metadata: Metadata;
  /** name is the canonical JSON field. */
  name: string;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** sha256 is the canonical JSON field. */
  sha256: string;
  /** sizeBytes is the canonical JSON field. */
  sizeBytes: number;
}

/** is the wire representation of the snapshot page schema. */
export interface SnapshotPage {
  /** items is the canonical JSON field. */
  items: readonly Snapshot[];
  /** nextCursor is the canonical JSON field. */
  nextCursor?: string;
}

/** is the wire representation of the spawn failure kind schema. */
export type SpawnFailureKind = "not_found" | "permission_denied" | "invalid_cwd" | "malformed_executable";

/** is the wire representation of the stream cancel frame schema. */
export interface StreamCancelFrame {
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** type is the canonical JSON field. */
  type: "cancel";
}

/** is the wire representation of the stream credit frame schema. */
export interface StreamCreditFrame {
  /** bytes is the canonical JSON field. */
  bytes: number;
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** type is the canonical JSON field. */
  type: "credit";
}

/** is the wire representation of the stream input frame schema. */
export interface StreamInputFrame {
  /** Ordered process standard-input bytes; empty only when endOfInput is true. */
  dataBase64: string;
  /** Closes process standard input after dataBase64; subsequent stdin frames are invalid. */
  endOfInput: boolean;
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** type is the canonical JSON field. */
  type: "stdin";
}

/** is the wire representation of the stream outcome frame schema. */
export interface StreamOutcomeFrame {
  /** outcome is the canonical JSON field. */
  outcome: ExecOutcome;
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** type is the canonical JSON field. */
  type: "outcome";
}

/** is the wire representation of the stream output frame schema. */
export interface StreamOutputFrame {
  /** dataBase64 is the canonical JSON field. */
  dataBase64: string;
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** stream is the canonical JSON field. */
  stream: string;
  /** type is the canonical JSON field. */
  type: "output";
}

/** is the wire representation of the stream signal frame schema. */
export interface StreamSignalFrame {
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** signal is the canonical JSON field. */
  signal: number;
  /** type is the canonical JSON field. */
  type: "signal";
}

/** is the wire representation of the streaming exec request schema. */
export interface StreamingExecRequest {
  /** command is the canonical JSON field. */
  command: Command;
  /** cwd is the canonical JSON field. */
  cwd?: WorkspacePath;
  /** deadlineMilliseconds is the canonical JSON field. */
  deadlineMilliseconds: number;
  /** environment is the canonical JSON field. */
  environment: StringMap;
  /** maximumOutputBytes is the canonical JSON field. */
  maximumOutputBytes: number;
  /** windowBytes is the canonical JSON field. */
  windowBytes: number;
}

/** is the wire representation of the string map schema. */
export type StringMap = Readonly<Record<string, string>>;

/** is the wire representation of the terminal frame schema. */
export type TerminalFrame = TerminalInputFrame | TerminalOutputFrame | TerminalResizeFrame | StreamCreditFrame | StreamCancelFrame | StreamOutcomeFrame;

/** is the wire representation of the terminal input frame schema. */
export interface TerminalInputFrame {
  /** dataBase64 is the canonical JSON field. */
  dataBase64: string;
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** type is the canonical JSON field. */
  type: "terminal_input";
}

/** is the wire representation of the terminal output frame schema. */
export interface TerminalOutputFrame {
  /** dataBase64 is the canonical JSON field. */
  dataBase64: string;
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** type is the canonical JSON field. */
  type: "terminal_output";
}

/** is the wire representation of the terminal resize frame schema. */
export interface TerminalResizeFrame {
  /** columns is the canonical JSON field. */
  columns: number;
  /** rows is the canonical JSON field. */
  rows: number;
  /** sequence is the canonical JSON field. */
  sequence: number;
  /** type is the canonical JSON field. */
  type: "resize";
}

/** is the wire representation of the terminal session schema. */
export interface TerminalSession {
  /** expiresAt is the canonical JSON field. */
  expiresAt: Timestamp;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** nextClientSequence is the canonical JSON field. */
  nextClientSequence: number;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
  /** state is the canonical JSON field. */
  state: SessionState;
  /** subprotocol is the canonical JSON field. */
  subprotocol: "secondbox.terminal.v1";
  /** websocketUrl is the canonical JSON field. */
  websocketUrl: string;
}

/** is the wire representation of the timestamp schema. */
export type Timestamp = string;

/** is the wire representation of the touch result schema. */
export interface TouchResult {
  /** generation is the canonical JSON field. */
  generation: number;
  /** lastActivityAt is the canonical JSON field. */
  lastActivityAt: Timestamp;
  /** sandboxId is the canonical JSON field. */
  sandboxId: OpaqueID;
}

/** is the wire representation of the update project request schema. */
export interface UpdateProjectRequest {
  /** name is the canonical JSON field. */
  name?: string;
  /** state is the canonical JSON field. */
  state?: ProjectState;
}

/** is the wire representation of the update runner pool request schema. */
export interface UpdateRunnerPoolRequest {
  /** architectures is the canonical JSON field. */
  architectures?: RunnerArchitectureList;
  /** capabilities is the canonical JSON field. */
  capabilities?: RunnerCapabilityList;
  /** capacityPolicy is the canonical JSON field. */
  capacityPolicy?: RunnerCapacityPolicy;
  /** state is the canonical JSON field. */
  state?: RunnerPoolState;
}

/** is the wire representation of the update service account request schema. */
export interface UpdateServiceAccountRequest {
  /** name is the canonical JSON field. */
  name?: string;
  /** profileGrants is the canonical JSON field. */
  profileGrants?: readonly ProfileName[];
  /** scopes is the canonical JSON field. */
  scopes?: readonly ServiceAccountScope[];
  /** state is the canonical JSON field. */
  state?: ServiceAccountState;
}

/** is the wire representation of the upload artifact request schema. */
export interface UploadArtifactRequest {
  /** content is the canonical JSON field. */
  content: string;
  /** mediaType is the canonical JSON field. */
  mediaType: string;
  /** metadata is the canonical JSON field. */
  metadata: Metadata;
  /** name is the canonical JSON field. */
  name: string;
  /** sha256 is the canonical JSON field. */
  sha256: string;
}

/** is the wire representation of the wait sandbox request schema. */
export interface WaitSandboxRequest {
  /** deadlineMilliseconds is the canonical JSON field. */
  deadlineMilliseconds: number;
  /** states is the canonical JSON field. */
  states: readonly SandboxState[];
}

/** is the wire representation of the workspace schema. */
export interface Workspace {
  /** createdAt is the canonical JSON field. */
  createdAt: Timestamp;
  /** currentSnapshotId is the canonical JSON field. */
  currentSnapshotId?: OpaqueID;
  /** generation is the canonical JSON field. */
  generation: number;
  /** id is the canonical JSON field. */
  id: OpaqueID;
  /** retainUntil is the canonical JSON field. */
  retainUntil?: Timestamp;
  /** retainedBytes is the canonical JSON field. */
  retainedBytes: number;
  /** updatedAt is the canonical JSON field. */
  updatedAt: Timestamp;
}

/** is the wire representation of the workspace path schema. */
export type WorkspacePath = string;

/** A JSON value accepted by the dependency-free transport helper. */
export type JSONValue = string | number | boolean | null | readonly JSONValue[] | {readonly [key: string]: JSONValue};

/** One canonical path, query, or header parameter. */
export interface OperationParameter {
  /** Exact parameter wire name. */
  readonly name: string;
  /** Parameter placement in the HTTP request. */
  readonly location: "path" | "query" | "header";
  /** Whether the contract requires the parameter. */
  readonly required: boolean;
  /** Component schema name or primitive wire type. */
  readonly schema: string;
}

/** One accepted request body representation. */
export interface OperationMediaType {
  /** Exact HTTP media type. */
  readonly contentType: string;
  /** Component schema name or primitive wire type. */
  readonly schema: string;
}

/** One declared operation response representation. */
export interface OperationResponse {
  /** OpenAPI response status or default. */
  readonly statusCode: string;
  /** Empty when the response has no body. */
  readonly contentType: string;
  /** Empty when the response has no body. */
  readonly schema: string;
}

/** Canonical transport metadata for one OpenAPI operation. */
export interface OperationMetadata {
  /** Stable OpenAPI operationId. */
  readonly operationId: string;
  /** Uppercase HTTP method. */
  readonly method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE";
  /** Versioned path with named placeholders. */
  readonly pathTemplate: string;
  /** Path, query, and header parameters. */
  readonly parameters: readonly OperationParameter[];
  /** Accepted request body representations. */
  readonly requestBody: readonly OperationMediaType[];
  /** Whether the operation requires a request body. */
  readonly requestBodyRequired: boolean;
  /** Every declared status and representation. */
  readonly responses: readonly OperationResponse[];
}

/** Stable operation metadata keyed by OpenAPI operationId. */
export const OPERATIONS = {
  /** The acquireSandboxLease OpenAPI operation. */
  acquireSandboxLease: {
    operationId: "acquireSandboxLease",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/leases",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "AcquireLeaseRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "Lease"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The cancelSandboxExecStream OpenAPI operation. */
  cancelSandboxExecStream: {
    operationId: "cancelSandboxExecStream",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/exec-streams/{execSessionId}:cancel",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "execSessionId", location: "path", required: true, schema: "OpaqueID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "202", contentType: "application/json", schema: "Operation"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The cancelSandboxTerminal OpenAPI operation. */
  cancelSandboxTerminal: {
    operationId: "cancelSandboxTerminal",
    method: "DELETE",
    pathTemplate: "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "terminalSessionId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "202", contentType: "application/json", schema: "Operation"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The checkpointSandbox OpenAPI operation. */
  checkpointSandbox: {
    operationId: "checkpointSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:checkpoint",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CheckpointSandboxRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "202", contentType: "application/json", schema: "Operation"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The closeSandboxPortSession OpenAPI operation. */
  closeSandboxPortSession: {
    operationId: "closeSandboxPortSession",
    method: "DELETE",
    pathTemplate: "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "portSessionId", location: "path", required: true, schema: "OpaqueID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "204", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The createAPIKey OpenAPI operation. */
  createAPIKey: {
    operationId: "createAPIKey",
    method: "POST",
    pathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
      {name: "serviceAccountId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateAPIKeyRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "CreateAPIKeyResponse"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The createProfile OpenAPI operation. */
  createProfile: {
    operationId: "createProfile",
    method: "POST",
    pathTemplate: "/v1/profiles",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateProfileRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "Profile"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The createProject OpenAPI operation. */
  createProject: {
    operationId: "createProject",
    method: "POST",
    pathTemplate: "/v1/projects",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateProjectRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "Project"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The createRunnerPool OpenAPI operation. */
  createRunnerPool: {
    operationId: "createRunnerPool",
    method: "POST",
    pathTemplate: "/v1/runner-pools",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateRunnerPoolRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "RunnerPool"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The createSandbox OpenAPI operation. */
  createSandbox: {
    operationId: "createSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateSandboxRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "Sandbox"},
      {statusCode: "202", contentType: "application/json", schema: "Operation"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "429", contentType: "", schema: ""},
    ],
  },
  /** The createSandboxDirectory OpenAPI operation. */
  createSandboxDirectory: {
    operationId: "createSandboxDirectory",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/directories",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateDirectoryRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "204", contentType: "", schema: ""},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The createSandboxExecStream OpenAPI operation. */
  createSandboxExecStream: {
    operationId: "createSandboxExecStream",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/exec-streams",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "StreamingExecRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "ExecStreamSession"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "429", contentType: "", schema: ""},
    ],
  },
  /** The createSandboxPortSession OpenAPI operation. */
  createSandboxPortSession: {
    operationId: "createSandboxPortSession",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/port-sessions",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreatePortSessionRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "PortSession"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "429", contentType: "", schema: ""},
    ],
  },
  /** The createSandboxSnapshot OpenAPI operation. */
  createSandboxSnapshot: {
    operationId: "createSandboxSnapshot",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/snapshots",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateSnapshotRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "Snapshot"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
      {statusCode: "429", contentType: "", schema: ""},
    ],
  },
  /** The createSandboxTerminal OpenAPI operation. */
  createSandboxTerminal: {
    operationId: "createSandboxTerminal",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/terminals",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateTerminalRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "TerminalSession"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "429", contentType: "", schema: ""},
    ],
  },
  /** The createServiceAccount OpenAPI operation. */
  createServiceAccount: {
    operationId: "createServiceAccount",
    method: "POST",
    pathTemplate: "/v1/projects/{projectId}/service-accounts",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "CreateServiceAccountRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "ServiceAccount"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The deleteArtifact OpenAPI operation. */
  deleteArtifact: {
    operationId: "deleteArtifact",
    method: "DELETE",
    pathTemplate: "/v1/artifacts/{artifactId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "artifactId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "204", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The deleteSandbox OpenAPI operation. */
  deleteSandbox: {
    operationId: "deleteSandbox",
    method: "DELETE",
    pathTemplate: "/v1/sandboxes/{sandboxId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "202", contentType: "application/json", schema: "TerminalSession"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The deleteSnapshot OpenAPI operation. */
  deleteSnapshot: {
    operationId: "deleteSnapshot",
    method: "DELETE",
    pathTemplate: "/v1/snapshots/{snapshotId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "snapshotId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "204", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The disableProfile OpenAPI operation. */
  disableProfile: {
    operationId: "disableProfile",
    method: "POST",
    pathTemplate: "/v1/profiles/{profileName}:disable",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "profileName", location: "path", required: true, schema: "ProfileName"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Profile"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The downloadArtifactContent OpenAPI operation. */
  downloadArtifactContent: {
    operationId: "downloadArtifactContent",
    method: "GET",
    pathTemplate: "/v1/artifacts/{artifactId}/content",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "artifactId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/octet-stream", schema: "string"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The drainSandbox OpenAPI operation. */
  drainSandbox: {
    operationId: "drainSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:drain",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "202", contentType: "application/json", schema: "Operation"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The executeSandboxCommand OpenAPI operation. */
  executeSandboxCommand: {
    operationId: "executeSandboxCommand",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/exec",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "BufferedExecRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "ExecOutcome"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "413", contentType: "", schema: ""},
      {statusCode: "429", contentType: "", schema: ""},
    ],
  },
  /** The getArtifact OpenAPI operation. */
  getArtifact: {
    operationId: "getArtifact",
    method: "GET",
    pathTemplate: "/v1/artifacts/{artifactId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "artifactId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Artifact"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getOperation OpenAPI operation. */
  getOperation: {
    operationId: "getOperation",
    method: "GET",
    pathTemplate: "/v1/operations/{operationId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "operationId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Operation"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getProfile OpenAPI operation. */
  getProfile: {
    operationId: "getProfile",
    method: "GET",
    pathTemplate: "/v1/profiles/{profileName}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "profileName", location: "path", required: true, schema: "ProfileName"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Profile"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getProject OpenAPI operation. */
  getProject: {
    operationId: "getProject",
    method: "GET",
    pathTemplate: "/v1/projects/{projectId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Project"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getRunner OpenAPI operation. */
  getRunner: {
    operationId: "getRunner",
    method: "GET",
    pathTemplate: "/v1/runners/{runnerId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "runnerId", location: "path", required: true, schema: "RunnerID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Runner"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getRunnerPool OpenAPI operation. */
  getRunnerPool: {
    operationId: "getRunnerPool",
    method: "GET",
    pathTemplate: "/v1/runner-pools/{runnerPoolName}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "runnerPoolName", location: "path", required: true, schema: "ProfileName"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "RunnerPool"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getSandbox OpenAPI operation. */
  getSandbox: {
    operationId: "getSandbox",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Sandbox"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getSandboxLease OpenAPI operation. */
  getSandboxLease: {
    operationId: "getSandboxLease",
    method: "GET",
    pathTemplate: "/v1/leases/{leaseId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "leaseId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Lease"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getSandboxPortSession OpenAPI operation. */
  getSandboxPortSession: {
    operationId: "getSandboxPortSession",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "portSessionId", location: "path", required: true, schema: "OpaqueID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "PortSession"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getServiceAccount OpenAPI operation. */
  getServiceAccount: {
    operationId: "getServiceAccount",
    method: "GET",
    pathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
      {name: "serviceAccountId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "ServiceAccount"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The getSnapshot OpenAPI operation. */
  getSnapshot: {
    operationId: "getSnapshot",
    method: "GET",
    pathTemplate: "/v1/snapshots/{snapshotId}",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "snapshotId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Snapshot"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The inspectSandbox OpenAPI operation. */
  inspectSandbox: {
    operationId: "inspectSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:inspect",
    parameters: [
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "SandboxInspection"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The listAPIKeys OpenAPI operation. */
  listAPIKeys: {
    operationId: "listAPIKeys",
    method: "GET",
    pathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
      {name: "serviceAccountId", location: "path", required: true, schema: "OpaqueID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "APIKeyPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The listProfiles OpenAPI operation. */
  listProfiles: {
    operationId: "listProfiles",
    method: "GET",
    pathTemplate: "/v1/profiles",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "ProfilePage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
    ],
  },
  /** The listProjects OpenAPI operation. */
  listProjects: {
    operationId: "listProjects",
    method: "GET",
    pathTemplate: "/v1/projects",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "ProjectPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
    ],
  },
  /** The listRunnerPools OpenAPI operation. */
  listRunnerPools: {
    operationId: "listRunnerPools",
    method: "GET",
    pathTemplate: "/v1/runner-pools",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "RunnerPoolPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
    ],
  },
  /** The listRunners OpenAPI operation. */
  listRunners: {
    operationId: "listRunners",
    method: "GET",
    pathTemplate: "/v1/runners",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
      {name: "pool", location: "query", required: false, schema: "ProfileName"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "RunnerPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
    ],
  },
  /** The listSandboxArtifacts OpenAPI operation. */
  listSandboxArtifacts: {
    operationId: "listSandboxArtifacts",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/artifacts",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "ArtifactPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The listSandboxDirectory OpenAPI operation. */
  listSandboxDirectory: {
    operationId: "listSandboxDirectory",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/directories",
    parameters: [
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "path", location: "query", required: true, schema: "WorkspacePath"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "DirectoryListing"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The listSandboxSnapshots OpenAPI operation. */
  listSandboxSnapshots: {
    operationId: "listSandboxSnapshots",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/snapshots",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "SnapshotPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The listSandboxes OpenAPI operation. */
  listSandboxes: {
    operationId: "listSandboxes",
    method: "GET",
    pathTemplate: "/v1/sandboxes",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "SandboxPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
    ],
  },
  /** The listServiceAccounts OpenAPI operation. */
  listServiceAccounts: {
    operationId: "listServiceAccounts",
    method: "GET",
    pathTemplate: "/v1/projects/{projectId}/service-accounts",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
      {name: "cursor", location: "query", required: false, schema: "string"},
      {name: "limit", location: "query", required: false, schema: "integer"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "ServiceAccountPage"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The pingSandbox OpenAPI operation. */
  pingSandbox: {
    operationId: "pingSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:ping",
    parameters: [
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "PingResult"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The readSandboxFile OpenAPI operation. */
  readSandboxFile: {
    operationId: "readSandboxFile",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/files",
    parameters: [
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "path", location: "query", required: true, schema: "WorkspacePath"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/octet-stream", schema: "string"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "413", contentType: "", schema: ""},
    ],
  },
  /** The reconnectSandboxTerminal OpenAPI operation. */
  reconnectSandboxTerminal: {
    operationId: "reconnectSandboxTerminal",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}",
    parameters: [
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "terminalSessionId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "TerminalSession"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The releaseSandboxLease OpenAPI operation. */
  releaseSandboxLease: {
    operationId: "releaseSandboxLease",
    method: "DELETE",
    pathTemplate: "/v1/leases/{leaseId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "leaseId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Lease"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
    ],
  },
  /** The removeSandboxPath OpenAPI operation. */
  removeSandboxPath: {
    operationId: "removeSandboxPath",
    method: "DELETE",
    pathTemplate: "/v1/sandboxes/{sandboxId}/directories",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "RemovePathRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "204", contentType: "", schema: ""},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The renewSandboxLease OpenAPI operation. */
  renewSandboxLease: {
    operationId: "renewSandboxLease",
    method: "POST",
    pathTemplate: "/v1/leases/{leaseId}:renew",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "leaseId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "RenewLeaseRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Lease"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The reviseProfile OpenAPI operation. */
  reviseProfile: {
    operationId: "reviseProfile",
    method: "POST",
    pathTemplate: "/v1/profiles/{profileName}:revise",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "profileName", location: "path", required: true, schema: "ProfileName"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "ReviseProfileRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Profile"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The revokeAPIKey OpenAPI operation. */
  revokeAPIKey: {
    operationId: "revokeAPIKey",
    method: "POST",
    pathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys/{apiKeyId}:revoke",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "apiKeyId", location: "path", required: true, schema: "OpaqueID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
      {name: "serviceAccountId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "APIKey"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The sandboxFileExists OpenAPI operation. */
  sandboxFileExists: {
    operationId: "sandboxFileExists",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/files:exists",
    parameters: [
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "path", location: "query", required: true, schema: "WorkspacePath"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "FileExistsResult"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The startSandbox OpenAPI operation. */
  startSandbox: {
    operationId: "startSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:start",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "202", contentType: "application/json", schema: "Operation"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
      {statusCode: "429", contentType: "", schema: ""},
    ],
  },
  /** The statSandboxFile OpenAPI operation. */
  statSandboxFile: {
    operationId: "statSandboxFile",
    method: "GET",
    pathTemplate: "/v1/sandboxes/{sandboxId}/files:stat",
    parameters: [
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "path", location: "query", required: true, schema: "WorkspacePath"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "FileStat"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The stopSandbox OpenAPI operation. */
  stopSandbox: {
    operationId: "stopSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:stop",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "202", contentType: "application/json", schema: "Operation"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The touchSandbox OpenAPI operation. */
  touchSandbox: {
    operationId: "touchSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:touch",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
    ],
    requestBodyRequired: false,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "TouchResult"},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
    ],
  },
  /** The updateProject OpenAPI operation. */
  updateProject: {
    operationId: "updateProject",
    method: "PATCH",
    pathTemplate: "/v1/projects/{projectId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "UpdateProjectRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Project"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The updateRunnerPool OpenAPI operation. */
  updateRunnerPool: {
    operationId: "updateRunnerPool",
    method: "PATCH",
    pathTemplate: "/v1/runner-pools/{runnerPoolName}",
    parameters: [
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "runnerPoolName", location: "path", required: true, schema: "ProfileName"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "UpdateRunnerPoolRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "RunnerPool"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The updateServiceAccount OpenAPI operation. */
  updateServiceAccount: {
    operationId: "updateServiceAccount",
    method: "PATCH",
    pathTemplate: "/v1/projects/{projectId}/service-accounts/{serviceAccountId}",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "If-Match", location: "header", required: true, schema: "string"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "projectId", location: "path", required: true, schema: "OpaqueID"},
      {name: "serviceAccountId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "UpdateServiceAccountRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "ServiceAccount"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "412", contentType: "", schema: ""},
    ],
  },
  /** The uploadSandboxArtifact OpenAPI operation. */
  uploadSandboxArtifact: {
    operationId: "uploadSandboxArtifact",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}/artifacts",
    parameters: [
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "multipart/form-data", schema: "UploadArtifactRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "201", contentType: "application/json", schema: "Artifact"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "413", contentType: "", schema: ""},
    ],
  },
  /** The waitForSandbox OpenAPI operation. */
  waitForSandbox: {
    operationId: "waitForSandbox",
    method: "POST",
    pathTemplate: "/v1/sandboxes/{sandboxId}:wait",
    parameters: [
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
    ],
    requestBody: [
      {contentType: "application/json", schema: "WaitSandboxRequest"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "Sandbox"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "408", contentType: "", schema: ""},
    ],
  },
  /** The writeSandboxFile OpenAPI operation. */
  writeSandboxFile: {
    operationId: "writeSandboxFile",
    method: "PUT",
    pathTemplate: "/v1/sandboxes/{sandboxId}/files",
    parameters: [
      {name: "Digest", location: "header", required: true, schema: "string"},
      {name: "Idempotency-Key", location: "header", required: true, schema: "string"},
      {name: "SecondBox-Generation", location: "header", required: true, schema: "integer"},
      {name: "SecondBox-Lease-ID", location: "header", required: false, schema: "OpaqueID"},
      {name: "X-Request-ID", location: "header", required: false, schema: "CorrelationID"},
      {name: "sandboxId", location: "path", required: true, schema: "OpaqueID"},
      {name: "path", location: "query", required: true, schema: "WorkspacePath"},
    ],
    requestBody: [
      {contentType: "application/octet-stream", schema: "string"},
    ],
    requestBodyRequired: true,
    responses: [
      {statusCode: "200", contentType: "application/json", schema: "FileWriteResult"},
      {statusCode: "400", contentType: "", schema: ""},
      {statusCode: "401", contentType: "", schema: ""},
      {statusCode: "403", contentType: "", schema: ""},
      {statusCode: "404", contentType: "", schema: ""},
      {statusCode: "409", contentType: "", schema: ""},
      {statusCode: "413", contentType: "", schema: ""},
    ],
  },
} as const satisfies Readonly<Record<string, OperationMetadata>>;

/** OperationID is one stable generated OpenAPI operationId. */
export type OperationID = keyof typeof OPERATIONS;

/** Wire values supplied to a generated operation. */
export interface TransportRequestOptions {
  /** Values replacing named path placeholders. */
  readonly pathParameters?: Readonly<Record<string, string>>;
  /** Values encoded into the query string. */
  readonly queryParameters?: Readonly<Record<string, string | number | boolean | readonly (string | number | boolean)[]>>;
  /** Operation headers such as Idempotency-Key and If-Match. */
  readonly headers?: Readonly<Record<string, string>>;
  /** Already encoded JSON, binary, or multipart body. */
  readonly body?: BodyInit;
  /** One media type declared by the selected operation. */
  readonly contentType?: string;
  /** Optional cancellation signal owned by the caller. */
  readonly signal?: AbortSignal;
}

/** A non-successful SecondBox HTTP response. */
export class SecondBoxAPIError extends Error {
  /** Raw response retained for bounded caller-controlled decoding. */
  public readonly response: Response;

  /** Creates a transport error without consuming its response body. */
  public constructor(response: Response) {
    super("SecondBox API request failed: status=" + response.status);
    this.name = "SecondBoxAPIError";
    this.response = response;
  }
}

/** Dependency-free fetch transport for the generated SecondBox contract. */
export class SecondBoxClient {
  private readonly baseUrl: URL;
  private readonly token: string;
  private readonly fetcher: typeof fetch;

  /** Validates the endpoint and requires an explicit fetch implementation. */
  public constructor(rawUrl: string, token: string, fetcher: typeof fetch) {
    const baseUrl = new URL(rawUrl);
    if ((baseUrl.protocol !== "http:" && baseUrl.protocol !== "https:") || baseUrl.search !== "" || baseUrl.hash !== "") {
      throw new Error("SecondBox client URL must be an absolute HTTP endpoint without query or fragment");
    }
    if (token === "") {
      throw new Error("SecondBox client service-account token is required");
    }
    this.baseUrl = baseUrl;
    this.token = token;
    this.fetcher = fetcher;
  }

  /** Sends one generated operation and returns the unconsumed successful response. */
  public async send(operation: OperationMetadata, options: TransportRequestOptions = {}): Promise<Response> {
    let path = operation.pathTemplate;
    for (const parameter of operation.parameters) {
      if (parameter.location !== "path") continue;
      const value = options.pathParameters?.[parameter.name];
      if (parameter.required && (value === undefined || value === "")) {
        throw new Error("SecondBox client missing required path parameter " + parameter.name + " for " + operation.operationId);
      }
      path = path.replaceAll("{" + parameter.name + "}", encodeURIComponent(value ?? ""));
    }
    if (path.includes("{")) {
      throw new Error("SecondBox client has unresolved path template " + path + " for " + operation.operationId);
    }

    const endpoint = new URL(path, this.baseUrl);
    for (const [name, value] of Object.entries(options.queryParameters ?? {})) {
      if (Array.isArray(value)) {
        for (const item of value) endpoint.searchParams.append(name, String(item));
      } else {
        endpoint.searchParams.set(name, String(value));
      }
    }
    let contentType = options.contentType;
    if (options.body !== undefined && contentType === undefined && operation.requestBody.length === 1) {
      contentType = operation.requestBody[0]?.contentType;
    }
    if (contentType !== undefined && !operation.requestBody.some((candidate) => candidate.contentType === contentType)) {
      throw new Error("SecondBox client content type " + contentType + " is not declared for " + operation.operationId);
    }
    const headers = new Headers(options.headers);
    headers.set("Authorization", "Bearer " + this.token);
    if (contentType !== undefined) headers.set("Content-Type", contentType);

    const response = await this.fetcher(endpoint, {
      method: operation.method,
      headers,
      body: options.body,
      signal: options.signal,
    });
    if (!response.ok) throw new SecondBoxAPIError(response);
    return response;
  }
}

/** Encodes a generated request model as an application/json body. */
export function encodeJSONBody(value: unknown): string {
  return JSON.stringify(value);
}

# Code generated from contracts/openapi/v1/secondbox.openapi.json (sha256 90ee902600bd204bb4fbbd22dda45b9b887fdd2fba3d7123172c0d1d968f9bf4); DO NOT EDIT.

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import BinaryIO, Final, Literal, Mapping, NotRequired, TypeAlias, TypedDict
from urllib import error, parse, request

CONTRACT_VERSION: Final[str] = "1.0.0"

JSONPrimitive: TypeAlias = str | int | float | bool | None
JSONValue: TypeAlias = JSONPrimitive | list["JSONValue"] | dict[str, "JSONValue"]

class APIKey(TypedDict):
    """is the wire representation of the api key schema."""

    createdAt: Timestamp
    expiresAt: NotRequired[Timestamp]
    id: OpaqueID
    lastUsedAt: NotRequired[Timestamp]
    name: str
    prefix: str
    revision: int
    revokedAt: NotRequired[Timestamp]
    scopes: list[ServiceAccountScope]
    serviceAccountId: OpaqueID
    state: APIKeyState


class APIKeyPage(TypedDict):
    """is the wire representation of the api key page schema."""

    items: list[APIKey]
    nextCursor: NotRequired[str]


# is the wire representation of the api key state schema.
APIKeyState: TypeAlias = Literal["active", "revoked", "expired"]

class AcquireLeaseRequest(TypedDict):
    """is the wire representation of the acquire lease request schema."""

    durationSeconds: int


class ArgvCommand(TypedDict):
    """is the wire representation of the argv command schema."""

    arguments: list[str]
    executable: str
    mode: Literal["argv"]


class Artifact(TypedDict):
    """is the wire representation of the artifact schema."""

    createdAt: Timestamp
    expiresAt: Timestamp
    generation: int
    id: OpaqueID
    mediaType: str
    metadata: Metadata
    name: str
    sandboxId: OpaqueID
    sha256: str
    sizeBytes: int


class ArtifactPage(TypedDict):
    """is the wire representation of the artifact page schema."""

    items: list[Artifact]
    nextCursor: NotRequired[str]


class BufferedExecRequest(TypedDict):
    """is the wire representation of the buffered exec request schema."""

    command: Command
    cwd: NotRequired[WorkspacePath]
    deadlineMilliseconds: int
    environment: StringMap
    maximumOutputBytes: int
    stdinBase64: NotRequired[str]


class CheckpointPolicy(TypedDict):
    """is the wire representation of the checkpoint policy schema."""

    artifactRetentionSeconds: int
    onStop: bool
    retentionSeconds: int
    snapshotLimit: int


class CheckpointSandboxRequest(TypedDict):
    """is the wire representation of the checkpoint sandbox request schema."""

    metadata: Metadata


# is the wire representation of the command schema.
Command: TypeAlias = "ShellCommand | ArgvCommand"

# is the wire representation of the correlation id schema.
CorrelationID: TypeAlias = str

class CreateAPIKeyRequest(TypedDict):
    """is the wire representation of the create api key request schema."""

    expiresAt: NotRequired[Timestamp]
    name: str
    scopes: list[ServiceAccountScope]


class CreateAPIKeyResponse(TypedDict):
    """is the wire representation of the create api key response schema."""

    apiKey: APIKey
    credential: str


class CreateDirectoryRequest(TypedDict):
    """is the wire representation of the create directory request schema."""

    path: WorkspacePath
    recursive: bool


class CreatePortSessionRequest(TypedDict):
    """is the wire representation of the create port session request schema."""

    durationSeconds: int
    name: str


class CreateProfileRequest(TypedDict):
    """is the wire representation of the create profile request schema."""

    name: ProfileName
    spec: ProfileRevisionSpec


class CreateProjectRequest(TypedDict):
    """is the wire representation of the create project request schema."""

    name: str


class CreateRunnerPoolRequest(TypedDict):
    """is the wire representation of the create runner pool request schema."""

    architectures: RunnerArchitectureList
    capabilities: RunnerCapabilityList
    capacityPolicy: RunnerCapacityPolicy
    name: ProfileName
    state: RunnerPoolState


class CreateSandboxRequest(TypedDict):
    """is the wire representation of the create sandbox request schema."""

    metadata: Metadata
    profile: ProfileName


class CreateServiceAccountRequest(TypedDict):
    """is the wire representation of the create service account request schema."""

    name: str
    profileGrants: list[ProfileName]
    scopes: list[ServiceAccountScope]


class CreateSnapshotRequest(TypedDict):
    """is the wire representation of the create snapshot request schema."""

    metadata: Metadata
    name: str


class CreateTerminalRequest(TypedDict):
    """is the wire representation of the create terminal request schema."""

    columns: int
    command: Command
    cwd: NotRequired[WorkspacePath]
    deadlineMilliseconds: int
    detachable: bool
    environment: StringMap
    rows: int


class DirectoryListing(TypedDict):
    """is the wire representation of the directory listing schema."""

    entries: list[FileStat]
    path: WorkspacePath


class ExecCancelled(TypedDict):
    """is the wire representation of the exec cancelled schema."""

    kind: Literal["cancelled"]
    output: ExecOutput


class ExecDeadlineExceeded(TypedDict):
    """is the wire representation of the exec deadline exceeded schema."""

    elapsedMilliseconds: int
    kind: Literal["deadline_exceeded"]
    output: ExecOutput


class ExecExited(TypedDict):
    """is the wire representation of the exec exited schema."""

    exitCode: int
    kind: Literal["exited"]
    output: ExecOutput
    signal: NotRequired[int]


class ExecInfrastructureFailed(TypedDict):
    """is the wire representation of the exec infrastructure failed schema."""

    kind: Literal["infrastructure_failed"]
    message: str
    reason: InfrastructureFailureKind
    retryable: bool


# is the wire representation of the exec outcome schema.
ExecOutcome: TypeAlias = "ExecExited | ExecSpawnFailed | ExecDeadlineExceeded | ExecCancelled | ExecOutputExhausted | ExecInfrastructureFailed"

class ExecOutput(TypedDict):
    """is the wire representation of the exec output schema."""

    stderrBase64: str
    stdoutBase64: str


class ExecOutputExhausted(TypedDict):
    """is the wire representation of the exec output exhausted schema."""

    kind: Literal["output_exhausted"]
    limitBytes: int
    output: ExecOutput


class ExecSpawnFailed(TypedDict):
    """is the wire representation of the exec spawn failed schema."""

    kind: Literal["spawn_failed"]
    message: str
    reason: SpawnFailureKind


# is the wire representation of the exec stream frame schema.
ExecStreamFrame: TypeAlias = "StreamInputFrame | StreamOutputFrame | StreamCreditFrame | StreamSignalFrame | StreamCancelFrame | StreamOutcomeFrame"

class ExecStreamSession(TypedDict):
    """is the wire representation of the exec stream session schema."""

    expiresAt: Timestamp
    generation: int
    id: OpaqueID
    sandboxId: OpaqueID
    state: SessionState
    subprotocol: Literal["secondbox.exec.v1"]
    websocketUrl: str


class ExecutionPolicy(TypedDict):
    """is the wire representation of the execution policy schema."""

    maximumBufferedOutputBytes: int
    maximumDeadlineMilliseconds: int
    maximumTransferBytes: int
    streamWindowBytes: int
    terminalDetachSeconds: int


class FileExistsResult(TypedDict):
    """is the wire representation of the file exists result schema."""

    exists: bool
    path: WorkspacePath


# is the wire representation of the file kind schema.
FileKind: TypeAlias = Literal["file", "directory", "symbolic_link"]

class FileStat(TypedDict):
    """is the wire representation of the file stat schema."""

    kind: FileKind
    modifiedAt: Timestamp
    path: WorkspacePath
    sizeBytes: int


class FileWriteResult(TypedDict):
    """is the wire representation of the file write result schema."""

    path: WorkspacePath
    sha256: str
    sizeBytes: int


# is the wire representation of the infrastructure failure kind schema.
InfrastructureFailureKind: TypeAlias = Literal["transport", "admission", "generation_fenced", "lease_fenced", "guest_agent", "execution_node", "service"]

class Instance(TypedDict):
    """is the wire representation of the instance schema."""

    createdAt: Timestamp
    generation: int
    id: OpaqueID
    readyAt: NotRequired[Timestamp]
    sandboxId: OpaqueID
    state: InstanceState
    stoppedAt: NotRequired[Timestamp]
    terminationReason: NotRequired[InstanceTerminationReason]
    updatedAt: Timestamp


# is the wire representation of the instance state schema.
InstanceState: TypeAlias = Literal["starting", "ready", "draining", "stopping", "stopped", "lost", "failed"]

# is the wire representation of the instance termination reason schema.
InstanceTerminationReason: TypeAlias = Literal["requested_drain", "requested_stop", "idle_timeout", "maximum_duration", "guest_shutdown", "resource_exhaustion", "guest_agent_lost", "runner_lost", "startup_failed", "fenced", "internal_failure"]

class Lease(TypedDict):
    """is the wire representation of the lease schema."""

    createdAt: Timestamp
    expiresAt: Timestamp
    generation: int
    id: OpaqueID
    sandboxId: OpaqueID
    state: LeaseState
    updatedAt: Timestamp


# is the wire representation of the lease state schema.
LeaseState: TypeAlias = Literal["active", "released", "expired", "fenced"]

class LifecyclePolicy(TypedDict):
    """is the wire representation of the lifecycle policy schema."""

    drainGraceSeconds: int
    idleSeconds: int
    initialState: str
    leaseSeconds: int
    maximumDurationSeconds: int


# is the wire representation of the metadata schema.
Metadata: TypeAlias = dict[str, str]

class NetworkDestination(TypedDict):
    """is the wire representation of the network destination schema."""

    cidr: NotRequired[str]
    domain: NotRequired[str]
    port: int
    protocol: str


class NetworkPolicy(TypedDict):
    """is the wire representation of the network policy schema."""

    destinations: list[NetworkDestination]
    mode: str


# is the wire representation of the opaque id schema.
OpaqueID: TypeAlias = str

class Operation(TypedDict):
    """is the wire representation of the operation schema."""

    completedAt: NotRequired[Timestamp]
    createdAt: Timestamp
    error: NotRequired[Problem]
    id: OpaqueID
    kind: OperationKind
    requestId: CorrelationID
    sandbox: NotRequired[Sandbox]
    sandboxId: OpaqueID
    startedAt: NotRequired[Timestamp]
    state: OperationState
    updatedAt: Timestamp


# is the wire representation of the operation kind schema.
OperationKind: TypeAlias = Literal["create", "start", "drain", "stop", "checkpoint", "delete", "cancel_exec", "cancel_terminal"]

# is the wire representation of the operation state schema.
OperationState: TypeAlias = Literal["pending", "running", "succeeded", "failed", "cancelled"]

class PingResult(TypedDict):
    """is the wire representation of the ping result schema."""

    generation: int
    healthy: bool
    observedAt: Timestamp
    sandboxId: OpaqueID


class PortPolicy(TypedDict):
    """is the wire representation of the port policy schema."""

    maximumSessionSeconds: int
    maximumSessions: int
    name: str
    port: int
    protocol: str


class PortSession(TypedDict):
    """is the wire representation of the port session schema."""

    createdAt: Timestamp
    endpoint: str
    expiresAt: Timestamp
    generation: int
    id: OpaqueID
    name: str
    protocol: str
    sandboxId: OpaqueID
    state: str


class Problem(TypedDict):
    """is the wire representation of the problem schema."""

    code: ProblemCode
    details: NotRequired[list[ProblemDetail]]
    requestId: CorrelationID
    retryable: bool
    status: int
    title: str
    type: str


# is the wire representation of the problem code schema.
ProblemCode: TypeAlias = Literal["invalid_request", "authentication_failed", "authorization_failed", "not_found", "idempotency_conflict", "precondition_failed", "state_conflict", "generation_fenced", "lease_fenced", "profile_unavailable", "quota_exceeded", "limit_exceeded", "guest_unavailable", "execution_node_unavailable", "dependency_unavailable", "internal_error", "wait_expired"]

class ProblemDetail(TypedDict):
    """is the wire representation of the problem detail schema."""

    field: str
    reason: str


class Profile(TypedDict):
    """is the wire representation of the profile schema."""

    createdAt: Timestamp
    currentRevision: ProfileRevision
    name: ProfileName
    revision: int
    state: ProfileState
    updatedAt: Timestamp


# is the wire representation of the profile name schema.
ProfileName: TypeAlias = str

class ProfilePage(TypedDict):
    """is the wire representation of the profile page schema."""

    items: list[Profile]
    nextCursor: NotRequired[str]


class ProfileRevision(TypedDict):
    """is the wire representation of the profile revision schema."""

    createdAt: Timestamp
    id: OpaqueID
    number: int
    spec: ProfileRevisionSpec


class ProfileRevisionSpec(TypedDict):
    """is the wire representation of the profile revision spec schema."""

    architecture: str
    backend: str
    checkpoint: CheckpointPolicy
    execution: ExecutionPolicy
    lifecycle: LifecyclePolicy
    network: NetworkPolicy
    pool: str
    ports: list[PortPolicy]
    resources: ResourcePolicy
    runtimeBundleDigest: str
    toolchainBundleDigest: str


# is the wire representation of the profile state schema.
ProfileState: TypeAlias = Literal["enabled", "disabled"]

class Project(TypedDict):
    """is the wire representation of the project schema."""

    createdAt: Timestamp
    id: OpaqueID
    name: str
    revision: int
    state: ProjectState
    updatedAt: Timestamp


class ProjectPage(TypedDict):
    """is the wire representation of the project page schema."""

    items: list[Project]
    nextCursor: NotRequired[str]


# is the wire representation of the project state schema.
ProjectState: TypeAlias = Literal["active", "disabled"]

class RemovePathRequest(TypedDict):
    """is the wire representation of the remove path request schema."""

    force: bool
    path: WorkspacePath
    recursive: bool


class RenewLeaseRequest(TypedDict):
    """is the wire representation of the renew lease request schema."""

    durationSeconds: int


class ResourcePolicy(TypedDict):
    """is the wire representation of the resource policy schema."""

    concurrentOperations: int
    cpuMillis: int
    memoryBytes: int
    processLimit: int
    workspaceBytes: int


class ReviseProfileRequest(TypedDict):
    """is the wire representation of the revise profile request schema."""

    spec: ProfileRevisionSpec


class Runner(TypedDict):
    """is the wire representation of the runner schema."""

    architectures: list[str]
    capabilities: list[str]
    capacity: dict[str, int]
    createdAt: Timestamp
    credentialState: str
    id: RunnerID
    lastSeenAt: NotRequired[Timestamp]
    name: str
    poolName: ProfileName
    protocolVersions: list[str]
    revision: int
    state: str
    updatedAt: Timestamp


# is the wire representation of the runner architecture list schema.
RunnerArchitectureList: TypeAlias = list[str]

# is the wire representation of the runner capability list schema.
RunnerCapabilityList: TypeAlias = list[str]

# is the wire representation of the runner capacity policy schema.
RunnerCapacityPolicy: TypeAlias = dict[str, int]

# is the wire representation of the runner id schema.
RunnerID: TypeAlias = str

class RunnerPage(TypedDict):
    """is the wire representation of the runner page schema."""

    items: list[Runner]
    nextCursor: NotRequired[str]


class RunnerPool(TypedDict):
    """is the wire representation of the runner pool schema."""

    architectures: RunnerArchitectureList
    capabilities: RunnerCapabilityList
    capacityPolicy: RunnerCapacityPolicy
    createdAt: Timestamp
    name: ProfileName
    readyRunnerCount: int
    revision: int
    state: RunnerPoolState
    updatedAt: Timestamp


class RunnerPoolPage(TypedDict):
    """is the wire representation of the runner pool page schema."""

    items: list[RunnerPool]
    nextCursor: NotRequired[str]


# is the wire representation of the runner pool state schema.
RunnerPoolState: TypeAlias = Literal["ready", "draining", "offline"]

class Sandbox(TypedDict):
    """is the wire representation of the sandbox schema."""

    createdAt: Timestamp
    deletedAt: NotRequired[Timestamp]
    desiredState: SandboxDesiredState
    generation: int
    id: OpaqueID
    instance: NotRequired[Instance]
    lastActivityAt: NotRequired[Timestamp]
    metadata: Metadata
    profile: ProfileName
    profileRevisionId: OpaqueID
    projectId: OpaqueID
    revision: int
    state: SandboxState
    updatedAt: Timestamp
    workspace: Workspace


# is the wire representation of the sandbox desired state schema.
SandboxDesiredState: TypeAlias = Literal["running", "stopped", "deleted"]

class SandboxInspection(TypedDict):
    """is the wire representation of the sandbox inspection schema."""

    activeSessions: int
    generation: int
    guestHealthy: bool
    observedAt: Timestamp
    sandboxId: OpaqueID


class SandboxPage(TypedDict):
    """is the wire representation of the sandbox page schema."""

    items: list[Sandbox]
    nextCursor: NotRequired[str]


# is the wire representation of the sandbox state schema.
SandboxState: TypeAlias = Literal["creating", "stopped", "starting", "ready", "draining", "stopping", "checkpointing", "failed", "deleting", "deleted"]

class ServiceAccount(TypedDict):
    """is the wire representation of the service account schema."""

    createdAt: Timestamp
    id: OpaqueID
    name: str
    profileGrants: list[ProfileName]
    projectId: OpaqueID
    revision: int
    scopes: list[ServiceAccountScope]
    state: ServiceAccountState
    updatedAt: Timestamp


class ServiceAccountPage(TypedDict):
    """is the wire representation of the service account page schema."""

    items: list[ServiceAccount]
    nextCursor: NotRequired[str]


# is the wire representation of the service account scope schema.
ServiceAccountScope: TypeAlias = Literal["sandbox:read", "sandbox:lifecycle", "sandbox:exec", "sandbox:files", "sandbox:artifacts", "sandbox:ports"]

# is the wire representation of the service account state schema.
ServiceAccountState: TypeAlias = Literal["active", "disabled"]

# is the wire representation of the session state schema.
SessionState: TypeAlias = Literal["open", "detached", "closing", "closed"]

class ShellCommand(TypedDict):
    """is the wire representation of the shell command schema."""

    command: str
    mode: Literal["shell"]


class Snapshot(TypedDict):
    """is the wire representation of the snapshot schema."""

    createdAt: Timestamp
    expiresAt: Timestamp
    generation: int
    id: OpaqueID
    metadata: Metadata
    name: str
    sandboxId: OpaqueID
    sha256: str
    sizeBytes: int


class SnapshotPage(TypedDict):
    """is the wire representation of the snapshot page schema."""

    items: list[Snapshot]
    nextCursor: NotRequired[str]


# is the wire representation of the spawn failure kind schema.
SpawnFailureKind: TypeAlias = Literal["not_found", "permission_denied", "invalid_cwd", "malformed_executable"]

class StreamCancelFrame(TypedDict):
    """is the wire representation of the stream cancel frame schema."""

    sequence: int
    type: Literal["cancel"]


class StreamCreditFrame(TypedDict):
    """is the wire representation of the stream credit frame schema."""

    bytes: int
    sequence: int
    type: Literal["credit"]


class StreamInputFrame(TypedDict):
    """is the wire representation of the stream input frame schema."""

    dataBase64: str
    endOfInput: bool
    sequence: int
    type: Literal["stdin"]


class StreamOutcomeFrame(TypedDict):
    """is the wire representation of the stream outcome frame schema."""

    outcome: ExecOutcome
    sequence: int
    type: Literal["outcome"]


class StreamOutputFrame(TypedDict):
    """is the wire representation of the stream output frame schema."""

    dataBase64: str
    sequence: int
    stream: str
    type: Literal["output"]


class StreamSignalFrame(TypedDict):
    """is the wire representation of the stream signal frame schema."""

    sequence: int
    signal: int
    type: Literal["signal"]


class StreamingExecRequest(TypedDict):
    """is the wire representation of the streaming exec request schema."""

    command: Command
    cwd: NotRequired[WorkspacePath]
    deadlineMilliseconds: int
    environment: StringMap
    maximumOutputBytes: int
    windowBytes: int


# is the wire representation of the string map schema.
StringMap: TypeAlias = dict[str, str]

# is the wire representation of the terminal frame schema.
TerminalFrame: TypeAlias = "TerminalInputFrame | TerminalOutputFrame | TerminalResizeFrame | StreamCreditFrame | StreamCancelFrame | StreamOutcomeFrame"

class TerminalInputFrame(TypedDict):
    """is the wire representation of the terminal input frame schema."""

    dataBase64: str
    sequence: int
    type: Literal["terminal_input"]


class TerminalOutputFrame(TypedDict):
    """is the wire representation of the terminal output frame schema."""

    dataBase64: str
    sequence: int
    type: Literal["terminal_output"]


class TerminalResizeFrame(TypedDict):
    """is the wire representation of the terminal resize frame schema."""

    columns: int
    rows: int
    sequence: int
    type: Literal["resize"]


class TerminalSession(TypedDict):
    """is the wire representation of the terminal session schema."""

    expiresAt: Timestamp
    generation: int
    id: OpaqueID
    nextClientSequence: int
    sandboxId: OpaqueID
    state: SessionState
    subprotocol: Literal["secondbox.terminal.v1"]
    websocketUrl: str


# is the wire representation of the timestamp schema.
Timestamp: TypeAlias = str

class TouchResult(TypedDict):
    """is the wire representation of the touch result schema."""

    generation: int
    lastActivityAt: Timestamp
    sandboxId: OpaqueID


class UpdateProjectRequest(TypedDict):
    """is the wire representation of the update project request schema."""

    name: NotRequired[str]
    state: NotRequired[ProjectState]


class UpdateRunnerPoolRequest(TypedDict):
    """is the wire representation of the update runner pool request schema."""

    architectures: NotRequired[RunnerArchitectureList]
    capabilities: NotRequired[RunnerCapabilityList]
    capacityPolicy: NotRequired[RunnerCapacityPolicy]
    state: NotRequired[RunnerPoolState]


class UpdateServiceAccountRequest(TypedDict):
    """is the wire representation of the update service account request schema."""

    name: NotRequired[str]
    profileGrants: NotRequired[list[ProfileName]]
    scopes: NotRequired[list[ServiceAccountScope]]
    state: NotRequired[ServiceAccountState]


class UploadArtifactRequest(TypedDict):
    """is the wire representation of the upload artifact request schema."""

    content: str
    mediaType: str
    metadata: Metadata
    name: str
    sha256: str


class WaitSandboxRequest(TypedDict):
    """is the wire representation of the wait sandbox request schema."""

    deadlineMilliseconds: int
    states: list[SandboxState]


class Workspace(TypedDict):
    """is the wire representation of the workspace schema."""

    createdAt: Timestamp
    currentSnapshotId: NotRequired[OpaqueID]
    generation: int
    id: OpaqueID
    retainUntil: NotRequired[Timestamp]
    retainedBytes: int
    updatedAt: Timestamp


# is the wire representation of the workspace path schema.
WorkspacePath: TypeAlias = str

@dataclass(frozen=True)
class OperationParameter:
    """Describes one canonical path, query, or header parameter."""

    name: str
    location: Literal["path", "query", "header"]
    required: bool
    schema: str


@dataclass(frozen=True)
class OperationMediaType:
    """Describes one accepted request body representation."""

    content_type: str
    schema: str


@dataclass(frozen=True)
class OperationResponse:
    """Describes one declared operation response representation."""

    status_code: str
    content_type: str
    schema: str


@dataclass(frozen=True)
class OperationMetadata:
    """Provides canonical transport metadata for one OpenAPI operation."""

    operation_id: str
    method: Literal["GET", "POST", "PUT", "PATCH", "DELETE"]
    path_template: str
    parameters: tuple[OperationParameter, ...]
    request_body: tuple[OperationMediaType, ...]
    request_body_required: bool
    responses: tuple[OperationResponse, ...]


OPERATIONS: Final[Mapping[str, OperationMetadata]] = {
    "acquireSandboxLease": OperationMetadata(
        operation_id="acquireSandboxLease",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/leases",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="AcquireLeaseRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="Lease"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "cancelSandboxExecStream": OperationMetadata(
        operation_id="cancelSandboxExecStream",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/exec-streams/{execSessionId}:cancel",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="execSessionId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="202", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "cancelSandboxTerminal": OperationMetadata(
        operation_id="cancelSandboxTerminal",
        method="DELETE",
        path_template="/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="terminalSessionId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="202", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "checkpointSandbox": OperationMetadata(
        operation_id="checkpointSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:checkpoint",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CheckpointSandboxRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="202", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "closeSandboxPortSession": OperationMetadata(
        operation_id="closeSandboxPortSession",
        method="DELETE",
        path_template="/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="portSessionId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="204", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "createAPIKey": OperationMetadata(
        operation_id="createAPIKey",
        method="POST",
        path_template="/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="serviceAccountId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateAPIKeyRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="CreateAPIKeyResponse"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "createProfile": OperationMetadata(
        operation_id="createProfile",
        method="POST",
        path_template="/v1/profiles",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateProfileRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="Profile"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "createProject": OperationMetadata(
        operation_id="createProject",
        method="POST",
        path_template="/v1/projects",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateProjectRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="Project"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "createRunnerPool": OperationMetadata(
        operation_id="createRunnerPool",
        method="POST",
        path_template="/v1/runner-pools",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateRunnerPoolRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="RunnerPool"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "createSandbox": OperationMetadata(
        operation_id="createSandbox",
        method="POST",
        path_template="/v1/sandboxes",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateSandboxRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="Sandbox"),
            OperationResponse(status_code="202", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="429", content_type="", schema=""),
        ),
    ),
    "createSandboxDirectory": OperationMetadata(
        operation_id="createSandboxDirectory",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/directories",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateDirectoryRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="204", content_type="", schema=""),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "createSandboxExecStream": OperationMetadata(
        operation_id="createSandboxExecStream",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/exec-streams",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="StreamingExecRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="ExecStreamSession"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="429", content_type="", schema=""),
        ),
    ),
    "createSandboxPortSession": OperationMetadata(
        operation_id="createSandboxPortSession",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/port-sessions",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreatePortSessionRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="PortSession"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="429", content_type="", schema=""),
        ),
    ),
    "createSandboxSnapshot": OperationMetadata(
        operation_id="createSandboxSnapshot",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/snapshots",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateSnapshotRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="Snapshot"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
            OperationResponse(status_code="429", content_type="", schema=""),
        ),
    ),
    "createSandboxTerminal": OperationMetadata(
        operation_id="createSandboxTerminal",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/terminals",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateTerminalRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="TerminalSession"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="429", content_type="", schema=""),
        ),
    ),
    "createServiceAccount": OperationMetadata(
        operation_id="createServiceAccount",
        method="POST",
        path_template="/v1/projects/{projectId}/service-accounts",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="CreateServiceAccountRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="ServiceAccount"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "deleteArtifact": OperationMetadata(
        operation_id="deleteArtifact",
        method="DELETE",
        path_template="/v1/artifacts/{artifactId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="artifactId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="204", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "deleteSandbox": OperationMetadata(
        operation_id="deleteSandbox",
        method="DELETE",
        path_template="/v1/sandboxes/{sandboxId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="202", content_type="application/json", schema="TerminalSession"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "deleteSnapshot": OperationMetadata(
        operation_id="deleteSnapshot",
        method="DELETE",
        path_template="/v1/snapshots/{snapshotId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="snapshotId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="204", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "disableProfile": OperationMetadata(
        operation_id="disableProfile",
        method="POST",
        path_template="/v1/profiles/{profileName}:disable",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="profileName", location="path", required=True, schema="ProfileName"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Profile"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "downloadArtifactContent": OperationMetadata(
        operation_id="downloadArtifactContent",
        method="GET",
        path_template="/v1/artifacts/{artifactId}/content",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="artifactId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/octet-stream", schema="string"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "drainSandbox": OperationMetadata(
        operation_id="drainSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:drain",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="202", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "executeSandboxCommand": OperationMetadata(
        operation_id="executeSandboxCommand",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/exec",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="BufferedExecRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="ExecOutcome"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="413", content_type="", schema=""),
            OperationResponse(status_code="429", content_type="", schema=""),
        ),
    ),
    "getArtifact": OperationMetadata(
        operation_id="getArtifact",
        method="GET",
        path_template="/v1/artifacts/{artifactId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="artifactId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Artifact"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getOperation": OperationMetadata(
        operation_id="getOperation",
        method="GET",
        path_template="/v1/operations/{operationId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="operationId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getProfile": OperationMetadata(
        operation_id="getProfile",
        method="GET",
        path_template="/v1/profiles/{profileName}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="profileName", location="path", required=True, schema="ProfileName"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Profile"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getProject": OperationMetadata(
        operation_id="getProject",
        method="GET",
        path_template="/v1/projects/{projectId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Project"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getRunner": OperationMetadata(
        operation_id="getRunner",
        method="GET",
        path_template="/v1/runners/{runnerId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="runnerId", location="path", required=True, schema="RunnerID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Runner"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getRunnerPool": OperationMetadata(
        operation_id="getRunnerPool",
        method="GET",
        path_template="/v1/runner-pools/{runnerPoolName}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="runnerPoolName", location="path", required=True, schema="ProfileName"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="RunnerPool"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getSandbox": OperationMetadata(
        operation_id="getSandbox",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Sandbox"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getSandboxLease": OperationMetadata(
        operation_id="getSandboxLease",
        method="GET",
        path_template="/v1/leases/{leaseId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="leaseId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Lease"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getSandboxPortSession": OperationMetadata(
        operation_id="getSandboxPortSession",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="portSessionId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="PortSession"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getServiceAccount": OperationMetadata(
        operation_id="getServiceAccount",
        method="GET",
        path_template="/v1/projects/{projectId}/service-accounts/{serviceAccountId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="serviceAccountId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="ServiceAccount"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "getSnapshot": OperationMetadata(
        operation_id="getSnapshot",
        method="GET",
        path_template="/v1/snapshots/{snapshotId}",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="snapshotId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Snapshot"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "inspectSandbox": OperationMetadata(
        operation_id="inspectSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:inspect",
        parameters=(
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="SandboxInspection"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "listAPIKeys": OperationMetadata(
        operation_id="listAPIKeys",
        method="GET",
        path_template="/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="serviceAccountId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="APIKeyPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "listProfiles": OperationMetadata(
        operation_id="listProfiles",
        method="GET",
        path_template="/v1/profiles",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="ProfilePage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
        ),
    ),
    "listProjects": OperationMetadata(
        operation_id="listProjects",
        method="GET",
        path_template="/v1/projects",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="ProjectPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
        ),
    ),
    "listRunnerPools": OperationMetadata(
        operation_id="listRunnerPools",
        method="GET",
        path_template="/v1/runner-pools",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="RunnerPoolPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
        ),
    ),
    "listRunners": OperationMetadata(
        operation_id="listRunners",
        method="GET",
        path_template="/v1/runners",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
            OperationParameter(name="pool", location="query", required=False, schema="ProfileName"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="RunnerPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
        ),
    ),
    "listSandboxArtifacts": OperationMetadata(
        operation_id="listSandboxArtifacts",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/artifacts",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="ArtifactPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "listSandboxDirectory": OperationMetadata(
        operation_id="listSandboxDirectory",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/directories",
        parameters=(
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="path", location="query", required=True, schema="WorkspacePath"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="DirectoryListing"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "listSandboxSnapshots": OperationMetadata(
        operation_id="listSandboxSnapshots",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/snapshots",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="SnapshotPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "listSandboxes": OperationMetadata(
        operation_id="listSandboxes",
        method="GET",
        path_template="/v1/sandboxes",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="SandboxPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
        ),
    ),
    "listServiceAccounts": OperationMetadata(
        operation_id="listServiceAccounts",
        method="GET",
        path_template="/v1/projects/{projectId}/service-accounts",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="cursor", location="query", required=False, schema="string"),
            OperationParameter(name="limit", location="query", required=False, schema="integer"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="ServiceAccountPage"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "pingSandbox": OperationMetadata(
        operation_id="pingSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:ping",
        parameters=(
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="PingResult"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "readSandboxFile": OperationMetadata(
        operation_id="readSandboxFile",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/files",
        parameters=(
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="path", location="query", required=True, schema="WorkspacePath"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/octet-stream", schema="string"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="413", content_type="", schema=""),
        ),
    ),
    "reconnectSandboxTerminal": OperationMetadata(
        operation_id="reconnectSandboxTerminal",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}",
        parameters=(
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="terminalSessionId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="TerminalSession"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "releaseSandboxLease": OperationMetadata(
        operation_id="releaseSandboxLease",
        method="DELETE",
        path_template="/v1/leases/{leaseId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="leaseId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Lease"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
        ),
    ),
    "removeSandboxPath": OperationMetadata(
        operation_id="removeSandboxPath",
        method="DELETE",
        path_template="/v1/sandboxes/{sandboxId}/directories",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="RemovePathRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="204", content_type="", schema=""),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "renewSandboxLease": OperationMetadata(
        operation_id="renewSandboxLease",
        method="POST",
        path_template="/v1/leases/{leaseId}:renew",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="leaseId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="RenewLeaseRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Lease"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "reviseProfile": OperationMetadata(
        operation_id="reviseProfile",
        method="POST",
        path_template="/v1/profiles/{profileName}:revise",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="profileName", location="path", required=True, schema="ProfileName"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="ReviseProfileRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Profile"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "revokeAPIKey": OperationMetadata(
        operation_id="revokeAPIKey",
        method="POST",
        path_template="/v1/projects/{projectId}/service-accounts/{serviceAccountId}/api-keys/{apiKeyId}:revoke",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="apiKeyId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="serviceAccountId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="APIKey"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "sandboxFileExists": OperationMetadata(
        operation_id="sandboxFileExists",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/files:exists",
        parameters=(
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="path", location="query", required=True, schema="WorkspacePath"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="FileExistsResult"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "startSandbox": OperationMetadata(
        operation_id="startSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:start",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="202", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
            OperationResponse(status_code="429", content_type="", schema=""),
        ),
    ),
    "statSandboxFile": OperationMetadata(
        operation_id="statSandboxFile",
        method="GET",
        path_template="/v1/sandboxes/{sandboxId}/files:stat",
        parameters=(
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="path", location="query", required=True, schema="WorkspacePath"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="FileStat"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "stopSandbox": OperationMetadata(
        operation_id="stopSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:stop",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="202", content_type="application/json", schema="Operation"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "touchSandbox": OperationMetadata(
        operation_id="touchSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:touch",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
        ),
        request_body_required=False,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="TouchResult"),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
        ),
    ),
    "updateProject": OperationMetadata(
        operation_id="updateProject",
        method="PATCH",
        path_template="/v1/projects/{projectId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="UpdateProjectRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Project"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "updateRunnerPool": OperationMetadata(
        operation_id="updateRunnerPool",
        method="PATCH",
        path_template="/v1/runner-pools/{runnerPoolName}",
        parameters=(
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="runnerPoolName", location="path", required=True, schema="ProfileName"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="UpdateRunnerPoolRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="RunnerPool"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "updateServiceAccount": OperationMetadata(
        operation_id="updateServiceAccount",
        method="PATCH",
        path_template="/v1/projects/{projectId}/service-accounts/{serviceAccountId}",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="If-Match", location="header", required=True, schema="string"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="projectId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="serviceAccountId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="UpdateServiceAccountRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="ServiceAccount"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="412", content_type="", schema=""),
        ),
    ),
    "uploadSandboxArtifact": OperationMetadata(
        operation_id="uploadSandboxArtifact",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}/artifacts",
        parameters=(
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="multipart/form-data", schema="UploadArtifactRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="201", content_type="application/json", schema="Artifact"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="413", content_type="", schema=""),
        ),
    ),
    "waitForSandbox": OperationMetadata(
        operation_id="waitForSandbox",
        method="POST",
        path_template="/v1/sandboxes/{sandboxId}:wait",
        parameters=(
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
        ),
        request_body=(
            OperationMediaType(content_type="application/json", schema="WaitSandboxRequest"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="Sandbox"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="408", content_type="", schema=""),
        ),
    ),
    "writeSandboxFile": OperationMetadata(
        operation_id="writeSandboxFile",
        method="PUT",
        path_template="/v1/sandboxes/{sandboxId}/files",
        parameters=(
            OperationParameter(name="Digest", location="header", required=True, schema="string"),
            OperationParameter(name="Idempotency-Key", location="header", required=True, schema="string"),
            OperationParameter(name="SecondBox-Generation", location="header", required=True, schema="integer"),
            OperationParameter(name="SecondBox-Lease-ID", location="header", required=False, schema="OpaqueID"),
            OperationParameter(name="X-Request-ID", location="header", required=False, schema="CorrelationID"),
            OperationParameter(name="sandboxId", location="path", required=True, schema="OpaqueID"),
            OperationParameter(name="path", location="query", required=True, schema="WorkspacePath"),
        ),
        request_body=(
            OperationMediaType(content_type="application/octet-stream", schema="string"),
        ),
        request_body_required=True,
        responses=(
            OperationResponse(status_code="200", content_type="application/json", schema="FileWriteResult"),
            OperationResponse(status_code="400", content_type="", schema=""),
            OperationResponse(status_code="401", content_type="", schema=""),
            OperationResponse(status_code="403", content_type="", schema=""),
            OperationResponse(status_code="404", content_type="", schema=""),
            OperationResponse(status_code="409", content_type="", schema=""),
            OperationResponse(status_code="413", content_type="", schema=""),
        ),
    ),
}


@dataclass(frozen=True)
class TransportResponse:
    """Contains a successful unconsumed-style SecondBox response as bounded bytes."""

    status_code: int
    headers: Mapping[str, str]
    body: bytes


class SecondBoxAPIError(Exception):
    """Carries a non-successful SecondBox response without approximating its problem type."""

    def __init__(self, status_code: int, headers: Mapping[str, str], body: bytes) -> None:
        super().__init__(f"SecondBox API request failed: status={status_code}")
        self.status_code = status_code
        self.headers = headers
        self.body = body


class SecondBoxClient:
    """Provides a dependency-free urllib transport over generated operation metadata."""

    def __init__(self, raw_url: str, token: str, timeout_seconds: float) -> None:
        endpoint = parse.urlsplit(raw_url)
        if endpoint.scheme not in ("http", "https") or endpoint.netloc == "" or endpoint.query != "" or endpoint.fragment != "":
            raise ValueError("SecondBox client URL must be an absolute HTTP endpoint without query or fragment")
        if token == "":
            raise ValueError("SecondBox client service-account token is required")
        if timeout_seconds <= 0:
            raise ValueError("SecondBox client timeout_seconds must be positive")
        self._base_url = raw_url.rstrip("/")
        self._token = token
        self._timeout_seconds = timeout_seconds

    def send(
        self,
        operation: OperationMetadata,
        *,
        path_parameters: Mapping[str, str] | None = None,
        query_parameters: Mapping[str, str | int | bool | list[str] | list[int]] | None = None,
        headers: Mapping[str, str] | None = None,
        body: bytes | BinaryIO | None = None,
        content_type: str | None = None,
    ) -> TransportResponse:
        """Sends one generated operation and returns its bounded successful response."""
        path = operation.path_template
        supplied_path_parameters = path_parameters or {}
        for parameter in operation.parameters:
            if parameter.location != "path":
                continue
            value = supplied_path_parameters.get(parameter.name)
            if parameter.required and (value is None or value == ""):
                raise ValueError(
                    f"SecondBox client missing required path parameter {parameter.name} for {operation.operation_id}"
                )
            path = path.replace("{" + parameter.name + "}", parse.quote(value or "", safe=""))
        if "{" in path:
            raise ValueError(f"SecondBox client has unresolved path template {path} for {operation.operation_id}")

        query = parse.urlencode(query_parameters or {}, doseq=True)
        endpoint = self._base_url + path
        if query != "":
            endpoint += "?" + query
        selected_content_type = content_type
        if body is not None and selected_content_type is None and len(operation.request_body) == 1:
            selected_content_type = operation.request_body[0].content_type
        if selected_content_type is not None and all(
            candidate.content_type != selected_content_type for candidate in operation.request_body
        ):
            raise ValueError(
                f"SecondBox client content type {selected_content_type} is not declared for {operation.operation_id}"
            )
        request_headers = dict(headers or {})
        request_headers["Authorization"] = "Bearer " + self._token
        if selected_content_type is not None:
            request_headers["Content-Type"] = selected_content_type
        payload = body.read() if hasattr(body, "read") else body
        call = request.Request(endpoint, data=payload, method=operation.method, headers=request_headers)
        try:
            with request.urlopen(call, timeout=self._timeout_seconds) as response:
                response_body = _read_bounded_response(response)
                return TransportResponse(
                    status_code=response.status,
                    headers=dict(response.headers.items()),
                    body=response_body,
                )
        except error.HTTPError as failure:
            failure_body = _read_bounded_response(failure)
            raise SecondBoxAPIError(
                status_code=failure.code,
                headers=dict(failure.headers.items()),
                body=failure_body,
            ) from failure


def encode_json_body(value: JSONValue) -> bytes:
    """Encodes a JSON value for a generated application/json request."""
    return json.dumps(value, separators=(",", ":")).encode("utf-8")


def _read_bounded_response(response: BinaryIO) -> bytes:
    maximum_response_bytes = 64 << 20
    body = response.read(maximum_response_bytes + 1)
    if len(body) > maximum_response_bytes:
        raise ValueError(f"SecondBox client response exceeds {maximum_response_bytes} bytes")
    return body

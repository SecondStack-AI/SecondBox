# Code generated from sandbox-service.openapi.json (sha256 78d7f7132f93eb4e415a11edb04184a05ac0b4952d8d97e582291c6d079ff679); DO NOT EDIT.

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

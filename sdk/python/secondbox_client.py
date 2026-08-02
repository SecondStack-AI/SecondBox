"""Small dependency-free SecondBox client for the trusted-caller API."""

from __future__ import annotations

import base64
import json
import secrets
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Mapping, Sequence
from urllib.error import HTTPError
from urllib.parse import quote, urlencode, urljoin, urlparse
from urllib.request import Request, urlopen


_OPERATIONS = {
    "acquireSandboxLease": ("POST", "/v1/sandboxes/{sandboxId}/leases"),
    "createSandboxSnapshot": ("POST", "/v1/sandboxes/{sandboxId}/snapshots"),
    "createProfile": ("POST", "/v1/profiles"),
    "createSandbox": ("POST", "/v1/sandboxes"),
    "deleteSandbox": ("DELETE", "/v1/sandboxes/{sandboxId}"),
    "deleteSnapshot": ("DELETE", "/v1/snapshots/{snapshotId}"),
    "drainSandbox": ("POST", "/v1/sandboxes/{sandboxId}:drain"),
    "executeSandboxCommand": ("POST", "/v1/sandboxes/{sandboxId}/exec"),
    "getOperation": ("GET", "/v1/operations/{operationId}"),
    "getSandbox": ("GET", "/v1/sandboxes/{sandboxId}"),
    "getSnapshot": ("GET", "/v1/snapshots/{snapshotId}"),
    "listSandboxes": ("GET", "/v1/sandboxes"),
    "listSandboxSnapshots": ("GET", "/v1/sandboxes/{sandboxId}/snapshots"),
    "releaseSandboxLease": ("DELETE", "/v1/leases/{leaseId}"),
    "renewSandboxLease": ("POST", "/v1/leases/{leaseId}:renew"),
    "restoreSandboxSnapshot": ("POST", "/v1/sandboxes/{sandboxId}:restore"),
    "startSandbox": ("POST", "/v1/sandboxes/{sandboxId}:start"),
    "stopSandbox": ("POST", "/v1/sandboxes/{sandboxId}:stop"),
    "waitForSandbox": ("POST", "/v1/sandboxes/{sandboxId}:wait"),
}


@dataclass(frozen=True)
class SecondBoxAPIError(Exception):
    status: int
    problem: Mapping[str, Any] | None
    body: bytes

    def __str__(self) -> str:
        code = self.problem.get("code") if self.problem else "unknown"
        return f"SecondBox API request failed: status={self.status} code={code}"




# The per-request bound the service enforces on waitForSandbox.
_MAXIMUM_WAIT_REQUEST_SECONDS = 55.0

# Keeps a very short Lease from busy-looping.
_DEFAULT_MINIMUM_RENEWAL_DELAY_SECONDS = 1.0


def new_idempotency_key() -> str:
    """Return one unguessable single-use request key."""
    return "sbk-" + secrets.token_hex(20)


def revision_etag(revision: int) -> str:
    """Render one Sandbox revision as its If-Match validator."""
    if revision < 1:
        raise ValueError("SecondBox Sandbox revision must be positive")
    return f'"revision-{revision}"'


def problem_code_of(error: BaseException | None) -> str:
    """Return the typed service problem code carried by the error, or ""."""
    if isinstance(error, SecondBoxAPIError) and error.problem:
        code = error.problem.get("code")
        if isinstance(code, str):
            return code
    return ""


@dataclass(frozen=True)
class ExecResult:
    """One terminal command outcome with its output already decoded."""

    kind: str
    stdout: bytes
    stderr: bytes
    exit_code: int | None = None
    signal: int | None = None
    elapsed_milliseconds: int = 0


class ExecFailure(Exception):
    """One terminal outcome that did not reach exit status zero."""

    def __init__(self, kind: str, message: str, result: "ExecResult") -> None:
        super().__init__(f"SecondBox command {kind}: {message}" if message else f"SecondBox command {kind}")
        self.kind = kind
        self.result = result


def _decode_output(output: Mapping[str, Any]) -> tuple[bytes, bytes]:
    def decode(name: str) -> bytes:
        encoded = output.get(name) or ""
        try:
            return base64.b64decode(encoded, validate=True)
        except (ValueError, TypeError) as error:
            raise ValueError(f"SecondBox command {name} is not canonical base64") from error

    return decode("stdoutBase64"), decode("stderrBase64")


def decode_exec_outcome(outcome: Mapping[str, Any]) -> ExecResult:
    """Decode the output any terminal outcome carries.

    A non-zero exit status and every non-exited outcome raise ExecFailure while
    still carrying the decoded output, because a command that failed usually
    explains itself on standard error.
    """
    kind = outcome.get("kind")
    stdout, stderr = _decode_output(outcome.get("output") or {})
    if kind == "exited":
        result = ExecResult(
            kind="exited",
            stdout=stdout,
            stderr=stderr,
            exit_code=int(outcome["exitCode"]),
            signal=outcome.get("signal"),
            elapsed_milliseconds=int(outcome.get("elapsedMilliseconds", 0)),
        )
        if result.exit_code != 0:
            raise ExecFailure("exited", f"exited with status {result.exit_code}", result)
        return result
    result = ExecResult(
        kind=str(kind),
        stdout=stdout,
        stderr=stderr,
        elapsed_milliseconds=int(outcome.get("elapsedMilliseconds", 0)),
    )
    if kind == "output_exhausted":
        raise ExecFailure(
            "output_exhausted",
            f"output limit of {outcome.get('limitBytes')} bytes was exhausted",
            result,
        )
    if kind in ("cancelled", "deadline_exceeded"):
        raise ExecFailure(str(kind), "", result)
    if kind in ("spawn_failed", "infrastructure_failed"):
        raise ExecFailure(str(kind), str(outcome.get("message", "")), result)
    raise ExecFailure("invalid", "SecondBox command returned an invalid outcome", result)


class SecondBoxClient:
    """HTTP client carrying the platform token and trusted ownership headers."""

    def __init__(
        self,
        base_url: str,
        platform_token: str,
        tenant_ref: str,
        subject_ref: str,
        *,
        timeout_seconds: float = 30,
    ) -> None:
        parsed = urlparse(base_url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ValueError("SecondBox client URL must be an absolute HTTP endpoint")
        if parsed.query or parsed.fragment:
            raise ValueError("SecondBox client URL must not contain a query or fragment")
        if not platform_token or not tenant_ref or not subject_ref:
            raise ValueError(
                "SecondBox client token, tenant reference, and subject reference are required"
            )
        self._base_url = base_url.rstrip("/") + "/"
        self._token = platform_token
        self._tenant_ref = tenant_ref
        self._subject_ref = subject_ref
        self._timeout_seconds = timeout_seconds

    def request(
        self,
        operation_id: str,
        *,
        path_parameters: Mapping[str, str] | None = None,
        query_parameters: Mapping[str, str] | Sequence[tuple[str, str]] | None = None,
        headers: Mapping[str, str] | None = None,
        body: Mapping[str, Any] | None = None,
    ) -> bytes:
        try:
            method, path = _OPERATIONS[operation_id]
        except KeyError as error:
            raise ValueError(f"unknown SecondBox operation {operation_id!r}") from error
        for name, value in (path_parameters or {}).items():
            path = path.replace("{" + name + "}", quote(value, safe=""))
        if "{" in path:
            raise ValueError(
                f"SecondBox operation {operation_id!r} is missing a path parameter"
            )
        endpoint = urljoin(self._base_url, path.lstrip("/"))
        if query_parameters:
            endpoint += "?" + urlencode(query_parameters)
        request_headers = {
            "Authorization": f"Bearer {self._token}",
            "X-SecondBox-Tenant-Ref": self._tenant_ref,
            "X-SecondBox-Subject-Ref": self._subject_ref,
            **(headers or {}),
        }
        encoded_body = None
        if body is not None:
            encoded_body = json.dumps(body, separators=(",", ":")).encode()
            request_headers["Content-Type"] = "application/json"
        request = Request(
            endpoint, data=encoded_body, headers=request_headers, method=method
        )
        try:
            with urlopen(request, timeout=self._timeout_seconds) as response:
                return response.read()
        except HTTPError as error:
            with error:
                response_body = error.read()
            try:
                problem = json.loads(response_body)
            except (UnicodeDecodeError, json.JSONDecodeError):
                problem = None
            raise SecondBoxAPIError(error.code, problem, response_body) from error

    def request_json(self, operation_id: str, **kwargs: Any) -> Mapping[str, Any]:
        return json.loads(self.request(operation_id, **kwargs))

    def create_sandbox(
        self,
        profile: str,
        metadata: Mapping[str, str],
        idempotency_key: str,
    ) -> Mapping[str, Any]:
        return self.request_json(
            "createSandbox",
            headers={"Idempotency-Key": idempotency_key},
            body={"profile": profile, "metadata": dict(metadata)},
        )

    def get_sandbox(self, sandbox_id: str) -> Mapping[str, Any]:
        return self.request_json(
            "getSandbox", path_parameters={"sandboxId": sandbox_id}
        )

    def list_sandboxes(
        self,
        *,
        limit: int = 100,
        cursor: str | None = None,
        metadata: Mapping[str, str] | None = None,
    ) -> Mapping[str, Any]:
        query: list[tuple[str, str]] = [("limit", str(limit))]
        if cursor:
            query.append(("cursor", cursor))
        for name, value in (metadata or {}).items():
            query.append(("metadata", f"{name}={value}"))
        return self.request_json("listSandboxes", query_parameters=query)

    def acquire_lease(
        self,
        sandbox_id: str,
        generation: int,
        duration_seconds: int,
        idempotency_key: str | None = None,
    ) -> Mapping[str, Any]:
        _require_duration(duration_seconds)
        return self.request_json(
            "acquireSandboxLease",
            path_parameters={"sandboxId": sandbox_id},
            headers={
                "SecondBox-Generation": str(generation),
                "Idempotency-Key": idempotency_key or new_idempotency_key(),
            },
            body={"durationSeconds": duration_seconds},
        )

    def renew_lease(
        self,
        lease_id: str,
        duration_seconds: int,
        idempotency_key: str | None = None,
    ) -> Mapping[str, Any]:
        if not lease_id:
            raise ValueError("SecondBox Lease renewal requires a Lease ID")
        _require_duration(duration_seconds)
        return self.request_json(
            "renewSandboxLease",
            path_parameters={"leaseId": lease_id},
            headers={"Idempotency-Key": idempotency_key or new_idempotency_key()},
            body={"durationSeconds": duration_seconds},
        )

    def release_lease(self, lease_id: str, idempotency_key: str | None = None) -> None:
        if not lease_id:
            raise ValueError("SecondBox Lease release requires a Lease ID")
        self.request(
            "releaseSandboxLease",
            path_parameters={"leaseId": lease_id},
            headers={"Idempotency-Key": idempotency_key or new_idempotency_key()},
        )

    def create_sandbox_handle(
        self,
        profile: str,
        metadata: Mapping[str, str] | None = None,
        *,
        source_snapshot_id: str | None = None,
        idempotency_key: str | None = None,
    ) -> tuple["SandboxHandle", Mapping[str, Any]]:
        """Admit one Sandbox and return a handle to its representation.

        The returned Sandbox is the freshly created resource, which is not yet
        ready; callers wait for the states they require.
        """
        if not profile:
            raise ValueError("SecondBox run requires a Profile name")
        body: dict[str, Any] = {"profile": profile, "metadata": dict(metadata or {})}
        if source_snapshot_id:
            body["sourceSnapshotId"] = source_snapshot_id
        operation = self.request_json(
            "createSandbox",
            headers={"Idempotency-Key": idempotency_key or new_idempotency_key()},
            body=body,
        )
        sandbox_id = operation.get("sandboxId")
        if not sandbox_id:
            raise ValueError("SecondBox Sandbox create returned no Sandbox reference")
        return SandboxHandle(self, self.get_sandbox(sandbox_id)), operation

    def run(
        self,
        profile: str,
        command: str,
        *,
        metadata: Mapping[str, str] | None = None,
        cwd: str | None = None,
        environment: Mapping[str, str] | None = None,
        stdin: bytes | None = None,
        deadline_milliseconds: int,
        maximum_output_bytes: int,
        ready_timeout_seconds: float = 300.0,
    ) -> tuple["SandboxHandle", ExecResult]:
        """Create a Sandbox, wait for it to become ready, and run one command.

        The Sandbox is deliberately left in place: no handle deletes a Sandbox
        implicitly. Callers dispose of the returned handle themselves.
        """
        handle, _ = self.create_sandbox_handle(profile, metadata)
        handle.wait_for(["ready"], deadline_seconds=ready_timeout_seconds)
        return handle, handle.execute(
            command,
            cwd=cwd,
            environment=environment,
            stdin=stdin,
            deadline_milliseconds=deadline_milliseconds,
            maximum_output_bytes=maximum_output_bytes,
        )


def _require_duration(duration_seconds: int) -> None:
    if duration_seconds < 1 or duration_seconds > 86400:
        raise ValueError(
            "SecondBox Lease duration must be from 1 second through 24 hours"
        )


class SandboxHandle:
    """Caller-owned Sandbox identity and its latest observed representation."""

    def __init__(self, client: SecondBoxClient, snapshot: Mapping[str, Any]) -> None:
        self._client = client
        self._snapshot = snapshot

    @property
    def snapshot(self) -> Mapping[str, Any]:
        return self._snapshot

    @property
    def id(self) -> str:
        return str(self._snapshot["id"])

    @property
    def generation(self) -> int:
        return int(self._snapshot["generation"])

    def refresh(self) -> Mapping[str, Any]:
        self._snapshot = self._client.get_sandbox(self.id)
        return self._snapshot

    def wait(self, states: Sequence[str], deadline_milliseconds: int) -> Mapping[str, Any]:
        if not states:
            raise ValueError("SecondBox Sandbox wait requires at least one state")
        if deadline_milliseconds < 1 or deadline_milliseconds > 60000:
            raise ValueError(
                "SecondBox Sandbox wait deadline must be from 1 through 60000 milliseconds"
            )
        self._snapshot = self._client.request_json(
            "waitForSandbox",
            path_parameters={"sandboxId": self.id},
            body={"states": list(states), "deadlineMilliseconds": deadline_milliseconds},
        )
        return self._snapshot

    def wait_for(
        self,
        states: Sequence[str],
        *,
        deadline_seconds: float,
        now: Any = time.monotonic,
    ) -> Mapping[str, Any]:
        """Block until the Sandbox reaches one of the supplied states.

        The service bounds a single wait request, so this issues repeated
        bounded waits against the caller's deadline and reports the last
        observed state when that deadline passes.
        """
        if not states:
            raise ValueError("SecondBox Sandbox wait requires at least one state")
        target = set(states)
        expiry = now() + deadline_seconds
        while True:
            if self._snapshot.get("state") in target:
                return self._snapshot
            remaining = expiry - now()
            if remaining <= 0:
                raise TimeoutError(
                    f"SecondBox Sandbox {self.id} did not reach {sorted(target)}: "
                    f"last state={self._snapshot.get('state')} generation={self.generation}"
                )
            bounded = min(remaining, _MAXIMUM_WAIT_REQUEST_SECONDS)
            try:
                self.wait(states, max(int(bounded * 1000), 1))
            except SecondBoxAPIError as error:
                if problem_code_of(error) != "wait_expired":
                    raise
                self.refresh()

    def execute(
        self,
        command: str,
        *,
        cwd: str | None = None,
        environment: Mapping[str, str] | None = None,
        stdin: bytes | None = None,
        deadline_milliseconds: int,
        maximum_output_bytes: int,
        lease_id: str | None = None,
        idempotency_key: str | None = None,
    ) -> ExecResult:
        """Run one bounded buffered shell command against the observed generation."""
        body: dict[str, Any] = {
            "command": {"mode": "shell", "command": command},
            "environment": dict(environment or {}),
            "deadlineMilliseconds": deadline_milliseconds,
            "maximumOutputBytes": maximum_output_bytes,
        }
        if cwd:
            body["cwd"] = cwd
        if stdin is not None:
            body["stdinBase64"] = base64.b64encode(stdin).decode()
        headers = {
            "SecondBox-Generation": str(self.generation),
            "Idempotency-Key": idempotency_key or new_idempotency_key(),
        }
        if lease_id:
            headers["SecondBox-Lease-ID"] = lease_id
        outcome = self._client.request_json(
            "executeSandboxCommand",
            path_parameters={"sandboxId": self.id},
            headers=headers,
            body=body,
        )
        return decode_exec_outcome(outcome)

    def acquire_lease(
        self, duration_seconds: int, idempotency_key: str | None = None
    ) -> Mapping[str, Any]:
        return self._client.acquire_lease(
            self.id, self.generation, duration_seconds, idempotency_key
        )

    def keep_lease(
        self,
        duration_seconds: int,
        minimum_delay_seconds: float = _DEFAULT_MINIMUM_RENEWAL_DELAY_SECONDS,
    ) -> "LeaseKeeper":
        """Acquire a Lease and renew it until the keeper is closed."""
        lease = self.acquire_lease(duration_seconds)
        keeper = LeaseKeeper(self._client, lease, duration_seconds, minimum_delay_seconds)
        keeper.start()
        return keeper


@dataclass
class LeaseKeeper:
    """Holds one Lease active by renewing it before it expires."""

    client: SecondBoxClient
    lease: Mapping[str, Any]
    duration_seconds: int
    minimum_delay_seconds: float = _DEFAULT_MINIMUM_RENEWAL_DELAY_SECONDS
    failure: BaseException | None = None
    _stop: threading.Event = field(default_factory=threading.Event)
    _thread: threading.Thread | None = None

    def start(self) -> None:
        self._thread = threading.Thread(target=self._renew, daemon=True)
        self._thread.start()

    @property
    def id(self) -> str:
        return str(self.lease["id"])

    def _renew(self) -> None:
        while not self._stop.is_set():
            if self._stop.wait(self._delay_seconds()):
                return
            try:
                self.lease = self.client.renew_lease(self.id, self.duration_seconds)
            except Exception as error:  # noqa: BLE001 - recorded for the caller
                self.failure = error
                return

    def _delay_seconds(self) -> float:
        """Renew at half the remaining life, with a floor."""
        expires_at = str(self.lease.get("expiresAt", ""))
        try:
            from datetime import datetime, timezone

            expiry = datetime.fromisoformat(expires_at.replace("Z", "+00:00"))
            remaining = (expiry - datetime.now(timezone.utc)).total_seconds()
        except ValueError:
            return self.minimum_delay_seconds
        if remaining <= 0:
            return self.minimum_delay_seconds
        return max(remaining / 2, self.minimum_delay_seconds)

    def close(self) -> None:
        """Stop renewal and release the Lease.

        A renewal that stopped is why the work failed; releasing a Lease the
        service has already fenced then fails too, and reporting that
        consequence instead of its cause sends the caller looking in the wrong
        place.
        """
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=30)
            self._thread = None
        release_error: BaseException | None = None
        try:
            self.client.release_lease(self.id)
        except Exception as error:  # noqa: BLE001 - reported below
            release_error = error
        if self.failure is not None:
            raise RuntimeError(
                f"SecondBox Lease renewal stopped: {self.failure}"
            ) from self.failure
        if release_error is not None:
            raise release_error

    def __enter__(self) -> "LeaseKeeper":
        return self

    def __exit__(self, *_exception: Any) -> None:
        self.close()

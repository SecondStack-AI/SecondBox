"""Small dependency-free SecondBox client for the trusted-caller API."""

from __future__ import annotations

import json
from dataclasses import dataclass
from typing import Any, Mapping
from urllib.error import HTTPError
from urllib.parse import quote, urlencode, urljoin, urlparse
from urllib.request import Request, urlopen


_OPERATIONS = {
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
    "listSandboxSnapshots": ("GET", "/v1/sandboxes/{sandboxId}/snapshots"),
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
        query_parameters: Mapping[str, str] | None = None,
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

import base64
import json
import threading
import time
import unittest
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from secondbox_client import (
    ExecFailure,
    LeaseKeeper,
    SandboxHandle,
    SecondBoxAPIError,
    SecondBoxClient,
    decode_exec_outcome,
    new_idempotency_key,
    problem_code_of,
    revision_etag,
)


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        assert self.path == "/v1/sandboxes/sandbox%2Fone"
        assert self.headers["Authorization"] == "Bearer token"
        assert self.headers["X-SecondBox-Tenant-Ref"] == "tenant"
        assert self.headers["X-SecondBox-Subject-Ref"] == "subject"
        body = json.dumps({"id": "sandbox/one"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format, *_args):
        return


class SecondBoxClientTest(unittest.TestCase):
    def test_get_sandbox_sends_trusted_caller_headers(self):
        server = ThreadingHTTPServer(("127.0.0.1", 0), _Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        client = SecondBoxClient(
            f"http://127.0.0.1:{server.server_port}",
            "token",
            "tenant",
            "subject",
        )
        self.assertEqual(client.get_sandbox("sandbox/one")["id"], "sandbox/one")





def _sandbox(state="ready"):
    return {
        "id": "sbx_one",
        "profile": "coding-environment",
        "profileRevisionId": "prv_1",
        "state": state,
        "desiredState": "running",
        "generation": 4,
        "workspace": {
            "id": "wsp_1",
            "generation": 4,
            "state": "ready",
            "sizeBytes": 1024,
            "createdAt": "2026-07-28T00:00:00Z",
            "updatedAt": "2026-07-28T00:00:00Z",
        },
        "metadata": {},
        "revision": 2,
        "createdAt": "2026-07-28T00:00:00Z",
        "updatedAt": "2026-07-28T00:00:00Z",
    }


def _exited(exit_code=0, stdout=b"", stderr=b""):
    return {
        "kind": "exited",
        "exitCode": exit_code,
        "elapsedMilliseconds": 5,
        "output": {
            "stdoutBase64": base64.b64encode(stdout).decode(),
            "stderrBase64": base64.b64encode(stderr).decode(),
        },
    }


def _lease(expires_in_seconds=60):
    expiry = datetime.now(timezone.utc) + timedelta(seconds=expires_in_seconds)
    return {
        "id": "lea_one",
        "sandboxId": "sbx_one",
        "generation": 4,
        "state": "active",
        "expiresAt": expiry.isoformat().replace("+00:00", "Z"),
        "createdAt": "2026-07-28T00:00:00Z",
        "updatedAt": "2026-07-28T00:00:00Z",
    }


class _ScriptedServer:
    """A server that answers from a caller-supplied route function."""

    def __init__(self, test, route):
        self.requests = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def _answer(self):
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length)) if length else None
                outer.requests.append(
                    {
                        "method": self.command,
                        "path": self.path,
                        "headers": {name.lower(): value for name, value in self.headers.items()},
                        "body": body,
                    }
                )
                status, payload = route(self.command, self.path, body)
                encoded = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(encoded)))
                self.end_headers()
                self.wfile.write(encoded)

            do_GET = _answer
            do_POST = _answer
            do_DELETE = _answer

            def log_message(self, _format, *_args):
                return

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        thread.start()
        test.addCleanup(self.server.server_close)
        test.addCleanup(self.server.shutdown)

    @property
    def client(self):
        return SecondBoxClient(
            f"http://127.0.0.1:{self.server.server_port}", "token", "tenant", "subject"
        )


class HelperTest(unittest.TestCase):
    def test_new_idempotency_key_is_prefixed_and_unique(self):
        keys = {new_idempotency_key() for _ in range(64)}
        self.assertEqual(len(keys), 64)
        self.assertTrue(all(key.startswith("sbk-") for key in keys))

    def test_revision_etag_matches_service_format(self):
        self.assertEqual(revision_etag(7), '"revision-7"')
        with self.assertRaises(ValueError):
            revision_etag(0)

    def test_problem_code_of_reads_typed_failures_only(self):
        typed = SecondBoxAPIError(409, {"code": "generation_fenced"}, b"")
        self.assertEqual(problem_code_of(typed), "generation_fenced")
        self.assertEqual(problem_code_of(ValueError("plain")), "")
        self.assertEqual(problem_code_of(None), "")

    def test_decode_exec_outcome_returns_output_alongside_failure(self):
        with self.assertRaises(ExecFailure) as raised:
            decode_exec_outcome(_exited(23, b"out", b"err"))
        self.assertEqual(raised.exception.kind, "exited")
        self.assertEqual(raised.exception.result.stdout, b"out")
        self.assertEqual(raised.exception.result.stderr, b"err")
        self.assertEqual(raised.exception.result.exit_code, 23)

    def test_decode_exec_outcome_succeeds_on_zero_exit(self):
        result = decode_exec_outcome(_exited(0, b"hello\n"))
        self.assertEqual(result.stdout, b"hello\n")
        self.assertEqual(result.exit_code, 0)

    def test_decode_exec_outcome_carries_truncated_output(self):
        outcome = {
            "kind": "output_exhausted",
            "limitBytes": 4,
            "output": {"stdoutBase64": base64.b64encode(b"abcd").decode(), "stderrBase64": ""},
        }
        with self.assertRaises(ExecFailure) as raised:
            decode_exec_outcome(outcome)
        self.assertEqual(raised.exception.result.stdout, b"abcd")
        self.assertIn("4 bytes", str(raised.exception))

    def test_decode_exec_outcome_rejects_non_canonical_base64(self):
        with self.assertRaises(ValueError):
            decode_exec_outcome(
                {"kind": "exited", "exitCode": 0, "output": {"stdoutBase64": "not base64!"}}
            )


class CompositionTest(unittest.TestCase):
    def test_wait_for_retries_after_the_service_reports_the_wait_expired(self):
        waits = []

        def route(method, path, _body):
            if path.endswith(":wait"):
                waits.append(path)
                if len(waits) == 1:
                    return 409, {"code": "wait_expired", "title": "Wait expired"}
                return 200, _sandbox("ready")
            return 200, _sandbox("starting")

        server = _ScriptedServer(self, route)
        handle = SandboxHandle(server.client, _sandbox("starting"))
        self.assertEqual(handle.wait_for(["ready"], deadline_seconds=10)["state"], "ready")
        self.assertGreaterEqual(len(waits), 2)

    def test_wait_for_reports_the_last_state_at_its_deadline(self):
        server = _ScriptedServer(self, lambda *_: (200, _sandbox("starting")))
        handle = SandboxHandle(server.client, _sandbox("starting"))
        with self.assertRaises(TimeoutError) as raised:
            handle.wait_for(["ready"], deadline_seconds=0)
        self.assertIn("last state=starting", str(raised.exception))

    def test_execute_applies_the_observed_generation_and_a_generated_key(self):
        server = _ScriptedServer(self, lambda *_: (200, _exited(0, b"hi\n")))
        handle = SandboxHandle(server.client, _sandbox())
        result = handle.execute(
            "echo hi", deadline_milliseconds=1000, maximum_output_bytes=1024, stdin=b"in"
        )
        self.assertEqual(result.stdout, b"hi\n")
        request = server.requests[-1]
        self.assertEqual(request["headers"]["secondbox-generation"], "4")
        self.assertTrue(request["headers"]["idempotency-key"].startswith("sbk-"))
        self.assertEqual(base64.b64decode(request["body"]["stdinBase64"]), b"in")

    def test_lease_routes_send_their_required_idempotency_key(self):
        server = _ScriptedServer(self, lambda *_: (200, _lease()))
        client = server.client
        client.acquire_lease("sbx_one", 4, 60)
        client.renew_lease("lea_one", 60)
        client.release_lease("lea_one")
        for request in server.requests:
            self.assertTrue(
                request["headers"].get("idempotency-key", "").startswith("sbk-"),
                f"{request['method']} {request['path']} carried no Idempotency-Key",
            )
        self.assertEqual(server.requests[0]["headers"]["secondbox-generation"], "4")

    def test_list_sandboxes_encodes_repeated_metadata_filters(self):
        server = _ScriptedServer(self, lambda *_: (200, {"items": []}))
        server.client.list_sandboxes(metadata={"secondbox.dev/name": "my-box"})
        self.assertIn("metadata=secondbox.dev%2Fname%3Dmy-box", server.requests[0]["path"])

    def test_run_creates_waits_and_executes_without_deleting(self):
        def route(method, path, _body):
            if path == "/v1/sandboxes" and method == "POST":
                return 200, {
                    "id": "op_1",
                    "sandboxId": "sbx_one",
                    "kind": "create",
                    "state": "pending",
                    "requestId": "req_1",
                    "createdAt": "2026-07-28T00:00:00Z",
                    "updatedAt": "2026-07-28T00:00:00Z",
                }
            if path.endswith("/exec"):
                return 200, _exited(0, b"hello\n")
            return 200, _sandbox("ready")

        server = _ScriptedServer(self, route)
        handle, result = server.client.run(
            "coding-environment",
            "echo hello",
            deadline_milliseconds=5000,
            maximum_output_bytes=1048576,
        )
        self.assertEqual(handle.id, "sbx_one")
        self.assertEqual(result.stdout, b"hello\n")
        methods = {request["method"] for request in server.requests}
        self.assertNotIn("DELETE", methods)

    def test_create_sandbox_handle_rejects_a_missing_sandbox_reference(self):
        server = _ScriptedServer(self, lambda *_: (200, {"id": "op_1", "sandboxId": ""}))
        with self.assertRaises(ValueError):
            server.client.create_sandbox_handle("coding-environment")


class LeaseKeeperTest(unittest.TestCase):
    def test_keeper_renews_until_closed_and_then_releases(self):
        counts = {"renew": 0, "release": 0}

        def route(method, path, _body):
            if method == "DELETE":
                counts["release"] += 1
                return 200, _lease()
            if path.endswith(":renew"):
                counts["renew"] += 1
            return 200, _lease(expires_in_seconds=0.02)

        server = _ScriptedServer(self, route)
        keeper = LeaseKeeper(server.client, _lease(0.02), 60, minimum_delay_seconds=0.005)
        keeper.start()
        deadline = time.monotonic() + 3
        while counts["renew"] < 2 and time.monotonic() < deadline:
            time.sleep(0.005)
        keeper.close()
        self.assertGreaterEqual(counts["renew"], 2)
        self.assertEqual(counts["release"], 1)
        self.assertIsNone(keeper.failure)

    def test_close_reports_the_renewal_failure_rather_than_its_consequence(self):
        def route(method, path, _body):
            if path.endswith(":renew"):
                return 409, {"code": "lease_fenced", "title": "Lease is fenced"}
            if method == "DELETE":
                return 409, {"code": "lease_fenced", "title": "Lease is inactive"}
            return 200, _lease()

        server = _ScriptedServer(self, route)
        keeper = LeaseKeeper(server.client, _lease(0), 60, minimum_delay_seconds=0.005)
        keeper.start()
        deadline = time.monotonic() + 3
        while keeper.failure is None and time.monotonic() < deadline:
            time.sleep(0.005)
        with self.assertRaises(RuntimeError) as raised:
            keeper.close()
        self.assertIn("Lease renewal stopped", str(raised.exception))
        self.assertEqual(problem_code_of(keeper.failure), "lease_fenced")


if __name__ == "__main__":
    unittest.main()

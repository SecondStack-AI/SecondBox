import json
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from secondbox_client import SecondBoxClient


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


if __name__ == "__main__":
    unittest.main()


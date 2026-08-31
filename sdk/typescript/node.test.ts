import assert from "node:assert/strict";
import { createServer } from "node:http";
import { once } from "node:events";
import test from "node:test";
import { WebSocketServer } from "ws";

import { SandboxHandle, SecondBox, problemCodeOf, type Sandbox } from "./client.ts";
import { createNodeTransports } from "./node.ts";
import {
  SecondBoxAPIError,
  SecondBoxClient,
  type TerminalSession,
} from "./transport.ts";

test("Node Exec transport authenticates and carries text frames", async () => {
  const server = createServer();
  const webSockets = new WebSocketServer({ noServer: true });
  let authority: Record<string, string | undefined> = {};
  let received = "";
  server.on("upgrade", (request, socket, head) => {
    authority = {
      authorization: request.headers.authorization,
      tenant: request.headers["x-secondbox-tenant-ref"] as string | undefined,
      subject: request.headers["x-secondbox-subject-ref"] as string | undefined,
      generation: request.headers["secondbox-generation"] as string | undefined,
    };
    webSockets.handleUpgrade(request, socket, head, (webSocket) => {
      webSockets.emit("connection", webSocket, request);
    });
  });
  webSockets.on("connection", (socket) => {
    socket.on("message", (data) => { received = data.toString(); });
    socket.send("server-frame");
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert(address && typeof address === "object");

  const transports = createNodeTransports({
    token: "token", tenantRef: "tenant", subjectRef: "subject",
  });
  const connection = await transports.exec.connect({
    websocketURL: `ws://127.0.0.1:${String(address.port)}/exec`,
    subprotocol: "secondbox.exec.v1",
    sandboxID: "sandbox-1",
    generation: 7,
    expiresAt: "2026-08-03T01:00:00Z",
  });
  assert.equal(await connection.receiveText(), "server-frame");
  await connection.sendText("client-frame");
  const deadline = Date.now() + 2_000;
  while (received === "" && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  assert.equal(received, "client-frame");
  assert.deepEqual(authority, {
    authorization: "Bearer token", tenant: "tenant", subject: "subject", generation: "7",
  });
  await connection.close();
  await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  webSockets.close();
});

test("Node Terminal transport forwards the replay cursor header only when provided", async () => {
  const server = createServer();
  const webSockets = new WebSocketServer({ noServer: true });
  const cursors: Array<string | undefined> = [];
  server.on("upgrade", (request, socket, head) => {
    cursors.push(request.headers["secondbox-terminal-after-sequence"] as string | undefined);
    webSockets.handleUpgrade(request, socket, head, (webSocket) => {
      webSockets.emit("connection", webSocket, request);
    });
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert(address && typeof address === "object");

  const transports = createNodeTransports({
    token: "token", tenantRef: "tenant", subjectRef: "subject",
  });
  const descriptor = {
    websocketURL: `ws://127.0.0.1:${String(address.port)}/terminal`,
    subprotocol: "secondbox.terminal.v1" as const,
    sandboxID: "sandbox-1",
    generation: 7,
    expiresAt: "2026-08-03T01:00:00Z",
  };
  const resumed = await transports.terminal.connect({ ...descriptor, afterSequence: 7 });
  const fresh = await transports.terminal.connect(descriptor);
  assert.deepEqual(cursors, ["7", undefined]);
  await resumed.close();
  await fresh.close();
  await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  webSockets.close();
});

test("Node Terminal replay eviction is typed like the Go SDK", async () => {
  const server = createServer();
  let cursor: string | undefined;
  server.on("upgrade", (request, socket) => {
    cursor = request.headers["secondbox-terminal-after-sequence"] as string | undefined;
    const body = JSON.stringify({
      type: "urn:secondbox:problem:terminal-replay-evicted",
      title: "Terminal replay sequence is no longer available",
      status: 409,
      code: "terminal_replay_evicted",
      requestId: "request-1",
      retryable: false,
      details: [],
    });
    socket.end(
      "HTTP/1.1 409 Conflict\r\n" +
      "Content-Type: application/problem+json\r\n" +
      `Content-Length: ${String(Buffer.byteLength(body))}\r\n` +
      "Connection: close\r\n" +
      "\r\n" +
      body,
    );
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert(address && typeof address === "object");

  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandboxFixture());
  const transports = createNodeTransports({
    token: "token", tenantRef: "tenant", subjectRef: "subject",
  });
  let failure: unknown;
  try {
    await handle.connectTerminalAfter(
      terminalSessionFixture(`ws://127.0.0.1:${String(address.port)}/terminal`),
      7,
      transports.terminal,
    );
  } catch (error) {
    failure = error;
  }
  assert.equal(cursor, "7");
  assert.equal(problemCodeOf(failure), "terminal_replay_evicted");
  await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
});

test("Node WebSocket handshake rejection without a Problem keeps the HTTP status", async () => {
  const server = createServer();
  server.on("upgrade", (_request, socket) => {
    const body = "<html>Bad Gateway</html>";
    socket.end(
      "HTTP/1.1 502 Bad Gateway\r\n" +
      "Content-Type: text/html\r\n" +
      `Content-Length: ${String(Buffer.byteLength(body))}\r\n` +
      "Connection: close\r\n" +
      "\r\n" +
      body,
    );
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert(address && typeof address === "object");

  const transports = createNodeTransports({
    token: "token", tenantRef: "tenant", subjectRef: "subject",
  });
  let failure: unknown;
  try {
    await transports.terminal.connect({
      websocketURL: `ws://127.0.0.1:${String(address.port)}/terminal`,
      subprotocol: "secondbox.terminal.v1",
      sandboxID: "sandbox-1",
      generation: 7,
      expiresAt: "2026-08-03T01:00:00Z",
    });
  } catch (error) {
    failure = error;
  }
  assert.ok(failure instanceof SecondBoxAPIError);
  assert.equal(failure.response.status, 502);
  assert.equal(await failure.response.text(), "<html>Bad Gateway</html>");
  await new Promise<void>((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
});

function sandboxFixture(): Sandbox {
  return {
    id: "sandbox-1",
    profile: "default",
    profileRevisionId: "profile-revision-1",
    egressContext: null,
    state: "ready",
    desiredState: "running",
    generation: 7,
    workspace: {
      id: "workspace-1",
      generation: 7,
      state: "ready",
      sizeBytes: 1_073_741_824,
      createdAt: "2026-07-28T00:00:00Z",
      updatedAt: "2026-07-28T00:00:00Z",
    },
    metadata: {},
    revision: 1,
    createdAt: "2026-07-28T00:00:00Z",
    updatedAt: "2026-07-28T00:00:00Z",
  };
}

function terminalSessionFixture(websocketUrl: string): TerminalSession {
  return {
    id: "term-1",
    sandboxId: "sandbox-1",
    generation: 7,
    state: "detached",
    websocketUrl,
    subprotocol: "secondbox.terminal.v1",
    streamWindowBytes: 65536,
    nextClientSequence: 4,
    expiresAt: "2026-08-03T01:00:00Z",
  };
}

test("Node transports reject incomplete authority and pre-aborted dials", async () => {
  assert.throws(
    () => createNodeTransports({ token: "", tenantRef: "tenant", subjectRef: "subject" }),
    /are required/,
  );
  const transports = createNodeTransports({ token: "token", tenantRef: "tenant", subjectRef: "subject" });
  const controller = new AbortController();
  controller.abort("cancelled");
  await assert.rejects(
    transports.exec.connect({
      websocketURL: "ws://127.0.0.1:1/exec",
      subprotocol: "secondbox.exec.v1",
      sandboxID: "sandbox-1",
      generation: 1,
      expiresAt: "2026-08-03T01:00:00Z",
    }, controller.signal),
    (error: unknown) => error instanceof DOMException && error.name === "AbortError",
  );
});

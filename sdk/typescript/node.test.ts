import assert from "node:assert/strict";
import { createServer } from "node:http";
import { once } from "node:events";
import test from "node:test";
import { WebSocketServer } from "ws";

import { createNodeTransports } from "./node.ts";

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

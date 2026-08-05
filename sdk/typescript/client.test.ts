import assert from "node:assert/strict";
import test from "node:test";

import {
  LeaseKeeper,
  OperationFailedError,
  PortTunnel,
  SandboxHandle,
  SecondBox,
  SecondBoxProblemError,
  type ExecStreamConnection,
  type Lease,
  type ExecStreamConnector,
  type PortTunnelConnection,
  type DirectPortDialer,
  type PortTunnelConnector,
  type Sandbox,
  type TerminalConnection,
  type TerminalConnector,
  decodeExecOutcome,
  newIdempotencyKey,
  problemCodeOf,
  revisionETag,
} from "./client.ts";
import { SecondBoxClient } from "./transport.ts";

test("requestJSON uses generated operation metadata", async () => {
  let requested = "";
  const fetcher: typeof fetch = async (input) => {
    requested = String(input);
    return Response.json(sandbox("ready"));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const result = await api.requestJSON<Sandbox>("getSandbox", {
    pathParameters: { sandboxId: "sandbox-1" },
  });
  assert.equal(requested, "https://secondbox.example/v1/sandboxes/sandbox-1");
  assert.equal(result.id, "sandbox-1");
});

test("requestJSON decodes structured SecondBox errors", async () => {
  const fetcher: typeof fetch = async () =>
    Response.json({
      type: "urn:secondbox:problem:not-found",
      title: "Sandbox not found",
      status: 404,
      code: "not_found",
      requestId: "request-1",
      retryable: false,
      details: [],
    }, { status: 404 });
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  await assert.rejects(
    api.requestJSON<Sandbox>("getSandbox", {
      pathParameters: { sandboxId: "missing" },
    }),
    (error: unknown) =>
      error instanceof SecondBoxProblemError &&
      error.status === 404 &&
      error.problem.code === "not_found",
  );
});

test("waitOperation reports structured terminal failure", async () => {
  const fetcher: typeof fetch = async () =>
    Response.json({
      id: "operation-1",
      sandboxId: "sandbox-1",
      kind: "start",
      state: "failed",
      requestId: "request-1",
      error: {
        type: "urn:secondbox:problem:state-conflict",
        title: "failed",
        status: 409,
        code: "state_conflict",
        requestId: "request-1",
        retryable: false,
        details: [],
      },
      createdAt: "2026-07-28T00:00:00Z",
      updatedAt: "2026-07-28T00:00:00Z",
    });
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  await assert.rejects(
    api.waitOperation("operation-1", { intervalMilliseconds: 1 }),
    (error: unknown) =>
      error instanceof OperationFailedError &&
      error.operation.error?.code === "state_conflict",
  );
});

test("waitOperation aborts polling", async () => {
  const fetcher: typeof fetch = async () =>
    Response.json({
      id: "operation-1",
      sandboxId: "sandbox-1",
      kind: "start",
      state: "running",
      requestId: "request-1",
      createdAt: "2026-07-28T00:00:00Z",
      updatedAt: "2026-07-28T00:00:00Z",
    });
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const controller = new AbortController();
  controller.abort(new Error("stop polling"));
  await assert.rejects(
    api.waitOperation("operation-1", {
      intervalMilliseconds: 10,
      signal: controller.signal,
    }),
    (error: unknown) => error instanceof DOMException && error.name === "AbortError",
  );
});

test("SandboxHandle sends generation and maps buffered output", async () => {
  let requestBody: unknown;
  let generation = "";
  const fetcher: typeof fetch = async (_input, init) => {
    generation = new Headers(init?.headers).get("SecondBox-Generation") ?? "";
    requestBody = JSON.parse(String(init?.body));
    return Response.json({
      kind: "exited",
      exitCode: 23,
      elapsedMilliseconds: 42,
      output: {
        stdoutBase64: btoa("hello"),
        stderrBase64: btoa("warning"),
      },
    });
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("ready"));
  const result = await handle.exec("false", {
    cwd: "project",
    environment: { EXPLICIT: "yes" },
    deadlineMilliseconds: 500,
    maximumOutputBytes: 4096,
  });
  assert.equal(generation, "7");
  assert.deepEqual(requestBody, {
    command: { mode: "shell", command: "false" },
    cwd: "project",
    environment: { EXPLICIT: "yes" },
    deadlineMilliseconds: 500,
    maximumOutputBytes: 4096,
  });
  assert.equal(result.kind, "exited");
  if (result.kind === "exited") {
    assert.equal(new TextDecoder().decode(result.stdout), "hello");
    assert.equal(result.exitCode, 23);
    assert.equal(result.elapsedMilliseconds, 42);
  }
});

test("SandboxHandle negotiates a streaming session", async () => {
  let requested = "";
  const fetcher: typeof fetch = async (input) => {
    requested = String(input);
    return Response.json({
      id: "exec-1",
      sandboxId: "sandbox-1",
      generation: 7,
      state: "open",
      websocketUrl: "wss://secondbox.example/exec-1",
      subprotocol: "secondbox.exec.v1",
      expiresAt: "2026-07-28T00:01:00Z",
    });
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("ready"));
  const session = await handle.createExecStream({
    command: { mode: "shell", command: "printf hello" },
    environment: {},
    deadlineMilliseconds: 500,
    maximumOutputBytes: 4096,
    windowBytes: 1024,
  }, "request-1");
  assert.equal(requested, "https://secondbox.example/v1/sandboxes/sandbox-1/exec-streams");
  assert.equal(session.subprotocol, "secondbox.exec.v1");
});

test("SandboxHandle attaches a sequenced streaming helper with explicit EOF", async () => {
  const sent: string[] = [];
  const received = [
    `{"type":"output","sequence":0,"stream":"stdout","dataBase64":"AQD/"}`,
    `{"type":"outcome","sequence":1,"outcome":{"kind":"exited","exitCode":0,"elapsedMilliseconds":0,"output":{"stdoutBase64":"","stderrBase64":""}}}`,
  ];
  let closed = false;
  const connection: ExecStreamConnection = {
    subprotocol: "secondbox.exec.v1",
    async sendText(payload) {
      sent.push(payload);
    },
    async receiveText() {
      const payload = received.shift();
      if (payload === undefined) throw new Error("missing test server frame");
      return payload;
    },
    async close() {
      closed = true;
    },
  };
  let descriptor: Parameters<ExecStreamConnector["connect"]>[0] | undefined;
  const connector: ExecStreamConnector = {
    async connect(value) {
      descriptor = value;
      return connection;
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"));
  const stream = await handle.connectExecStream(
    {
      id: "exec-1",
      sandboxId: "sandbox-1",
      generation: 7,
      state: "open",
      websocketUrl: "wss://secondbox.example/v1/exec-streams/exec-1",
      subprotocol: "secondbox.exec.v1",
      expiresAt: "2026-07-28T00:01:00Z",
    },
    connector,
  );
  assert.deepEqual(descriptor, {
    websocketURL: "wss://secondbox.example/v1/exec-streams/exec-1",
    subprotocol: "secondbox.exec.v1",
    sandboxID: "sandbox-1",
    generation: 7,
    expiresAt: "2026-07-28T00:01:00Z",
  });

  await stream.sendInput(new Uint8Array([0, 1, 255]));
  await stream.closeInput();
  await assert.rejects(
    stream.sendInput(new Uint8Array([2])),
    /standard input is already closed/,
  );
  await stream.grantOutput(4096);
  await stream.cancel();
  assert.deepEqual(sent.map((payload) => JSON.parse(payload)), [
    { type: "stdin", sequence: 0, dataBase64: "AAH/", endOfInput: false },
    { type: "stdin", sequence: 1, dataBase64: "", endOfInput: true },
    { type: "credit", sequence: 2, bytes: 4096 },
    { type: "cancel", sequence: 3 },
  ]);

  const output = await stream.receive();
  assert.equal(output.type, "output");
  const outcome = await stream.receive();
  assert.equal(outcome.type, "outcome");
  await assert.rejects(stream.receive(), /stream is terminal/);
  await assert.rejects(stream.grantOutput(1), /stream is terminal/);
  await stream.close();
  assert.equal(closed, true);
});

test("SandboxHandle rejects a mismatched streaming session before connector mutation", async () => {
  let connected = false;
  const connector: ExecStreamConnector = {
    async connect() {
      connected = true;
      throw new Error("connector must not run");
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"));
  await assert.rejects(
    handle.connectExecStream(
      {
        id: "exec-1",
        sandboxId: "different-sandbox",
        generation: 7,
        state: "open",
        websocketUrl: "wss://secondbox.example/exec-1",
        subprotocol: "secondbox.exec.v1",
        expiresAt: "2026-07-28T00:01:00Z",
      },
      connector,
    ),
    /does not match/,
  );
  assert.equal(connected, false);
});

test("SandboxHandle attaches a sequenced binary-safe Terminal helper", async () => {
  const sent: string[] = [];
  const received = [
    `{"type":"terminal_attached","nextClientSequence":9}`,
    `{"type":"terminal_output","sequence":0,"dataBase64":"AAH+/w=="}`,
    `{"type":"outcome","sequence":1,"outcome":{"kind":"cancelled"}}`,
  ];
  let closed = false;
  const connection: TerminalConnection = {
    subprotocol: "secondbox.terminal.v1",
    async sendText(payload) {
      sent.push(payload);
    },
    async receiveText() {
      const payload = received.shift();
      if (payload === undefined) throw new Error("missing test Terminal frame");
      return payload;
    },
    async close() {
      closed = true;
    },
  };
  let descriptor: Parameters<TerminalConnector["connect"]>[0] | undefined;
  const connector: TerminalConnector = {
    async connect(value) {
      descriptor = value;
      return connection;
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"));
  const terminal = await handle.connectTerminal(
    {
      id: "term-1",
      sandboxId: "sandbox-1",
      generation: 7,
      state: "detached",
      websocketUrl: "wss://secondbox.example/v1/sandboxes/sandbox-1/terminals/term-1",
      subprotocol: "secondbox.terminal.v1",
      streamWindowBytes: 65536,
      nextClientSequence: 4,
      expiresAt: "2026-07-28T00:01:00Z",
    },
    connector,
  );
  assert.deepEqual(descriptor, {
    websocketURL: "wss://secondbox.example/v1/sandboxes/sandbox-1/terminals/term-1",
    subprotocol: "secondbox.terminal.v1",
    sandboxID: "sandbox-1",
    generation: 7,
    expiresAt: "2026-07-28T00:01:00Z",
  });

  await terminal.grantOutput(4096);
  await terminal.resize(40, 120);
  await terminal.sendInput(new Uint8Array([0, 1, 254, 255]));
  await terminal.cancel();
  assert.deepEqual(sent.map((payload) => JSON.parse(payload)), [
    { type: "credit", sequence: 9, bytes: 4096 },
    { type: "resize", sequence: 10, rows: 40, columns: 120 },
    { type: "terminal_input", sequence: 11, dataBase64: "AAH+/w==" },
    { type: "cancel", sequence: 12 },
  ]);

  const output = await terminal.receive();
  assert.equal(output.type, "terminal_output");
  const outcome = await terminal.receive();
  assert.equal(outcome.type, "outcome");
  await assert.rejects(terminal.receive(), /Terminal is terminal/);
  await assert.rejects(terminal.sendInput(new Uint8Array([2])), /Terminal is terminal/);
  await terminal.close();
  assert.equal(closed, true);
});

test("SandboxHandle gets and cancels one stable Terminal session", async () => {
  const requests: Request[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = new Request(input, init);
    requests.push(request);
    return Response.json({
      id: "term-1",
      sandboxId: "sandbox-1",
      generation: 7,
      state: request.method === "DELETE" ? "closing" : "detached",
      websocketUrl: "wss://secondbox.example/v1/sandboxes/sandbox-1/terminals/term-1",
      subprotocol: "secondbox.terminal.v1",
      expiresAt: "2026-07-28T00:01:00Z",
    });
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("ready"));
  const detached = await handle.getTerminal("term-1");
  const closing = await handle.cancelTerminal("term-1", "cancel-terminal-1");
  assert.equal(detached.state, "detached");
  assert.equal(closing.state, "closing");
  assert.equal(requests[0]?.method, "GET");
  assert.equal(requests[1]?.method, "DELETE");
  assert.equal(requests[0]?.headers.get("SecondBox-Generation"), "7");
  assert.equal(requests[1]?.headers.get("Idempotency-Key"), "cancel-terminal-1");
});

test("SandboxHandle creates, gets, and closes one authenticated PortSession", async () => {
  const requests: Request[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = new Request(input, init);
    requests.push(request);
    if (request.method === "DELETE") return new Response(null, { status: 204 });
    return Response.json({
      id: "port-1",
      sandboxId: "sandbox-1",
      generation: 7,
      name: "ssh",
      protocol: "tcp",
      transport: "relay",
      endpoint: "wss://secondbox.example/v1/port-sessions/port-1#credential",
      state: "open",
      createdAt: "2026-07-28T00:00:00Z",
      expiresAt: "2026-07-28T00:01:00Z",
    });
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("ready"), "lease-1");

  const created = await handle.createPortSession(
    { name: "ssh", durationSeconds: 60 },
    "create-port-1",
  );
  const current = await handle.getPortSession("port-1");
  await handle.closePortSession("port-1", "close-port-1");

  assert.equal(created.name, "ssh");
  assert.equal(current.id, "port-1");
  assert.equal(requests[0]?.method, "POST");
  assert.deepEqual(await requests[0]?.json(), { name: "ssh", durationSeconds: 60 });
  assert.equal(requests[0]?.headers.get("SecondBox-Generation"), "7");
  assert.equal(requests[0]?.headers.get("SecondBox-Lease-ID"), "lease-1");
  assert.equal(requests[0]?.headers.get("Idempotency-Key"), "create-port-1");
  assert.equal(requests[1]?.method, "GET");
  assert.equal(requests[2]?.method, "DELETE");
  assert.equal(requests[2]?.headers.get("Idempotency-Key"), "close-port-1");
});

test("SandboxHandle acquires and API renews and releases a generation Lease", async () => {
  const requests: Request[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const request = new Request(input, init);
    requests.push(request);
    return Response.json({
      id: "lease-1",
      sandboxId: "sandbox-1",
      generation: 7,
      state: request.method === "DELETE" ? "released" : "active",
      expiresAt: "2026-07-28T00:01:00Z",
      createdAt: "2026-07-28T00:00:00Z",
      updatedAt: "2026-07-28T00:00:00Z",
    });
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("ready"));

  const acquired = await handle.acquireLease(60, "acquire-lease-1");
  const current = await api.getLease("lease-1");
  const renewed = await api.renewLease("lease-1", 90, "renew-lease-1");
  const released = await api.releaseLease("lease-1", "release-lease-1");

  assert.equal(acquired.id, "lease-1");
  assert.equal(current.state, "active");
  assert.equal(renewed.state, "active");
  assert.equal(released.state, "released");
  assert.equal(requests[0]?.method, "POST");
  assert.equal(requests[0]?.headers.get("SecondBox-Generation"), "7");
  assert.equal(requests[0]?.headers.get("Idempotency-Key"), "acquire-lease-1");
  assert.deepEqual(await requests[0]?.json(), { durationSeconds: 60 });
  assert.equal(requests[1]?.method, "GET");
  assert.equal(requests[2]?.method, "POST");
  assert.equal(requests[2]?.headers.get("Idempotency-Key"), "renew-lease-1");
  assert.deepEqual(await requests[2]?.json(), { durationSeconds: 90 });
  assert.equal(requests[3]?.method, "DELETE");
  assert.equal(requests[3]?.headers.get("Idempotency-Key"), "release-lease-1");
});

test("SandboxHandle attaches an authenticated binary Port tunnel", async () => {
  const sent: Uint8Array[] = [];
  const received = [new Uint8Array([4, 5, 6])];
  let closed = false;
  const connection: PortTunnelConnection = {
    subprotocol: "secondbox.port.v1",
    async sendBinary(payload) {
      sent.push(payload);
    },
    async receiveBinary() {
      const payload = received.shift();
      if (payload === undefined) throw new Error("missing test Port tunnel frame");
      return payload;
    },
    async close() {
      closed = true;
    },
  };
  let descriptor: Parameters<PortTunnelConnector["connect"]>[0] | undefined;
  const connector: PortTunnelConnector = {
    async connect(value) {
      descriptor = value;
      return connection;
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"), "lease-1");
  const tunnel = await handle.connectPortTunnel(
    {
      id: "port-1",
      sandboxId: "sandbox-1",
      generation: 7,
      name: "ssh",
      protocol: "tcp",
      transport: "relay",
      endpoint: "wss://secondbox.example/v1/port-sessions/port-1#single-use-token",
      state: "open",
      createdAt: "2026-07-28T00:00:00Z",
      expiresAt: "2026-07-28T00:01:00Z",
    },
    connector,
  );

  assert(tunnel instanceof PortTunnel);
  assert.deepEqual(descriptor, {
    websocketURL: "wss://secondbox.example/v1/port-sessions/port-1",
    subprotocols: [
      "secondbox.port.v1",
      "secondbox.port.token.single-use-token",
    ],
    sandboxID: "sandbox-1",
    generation: 7,
    expiresAt: "2026-07-28T00:01:00Z",
  });
  const outbound = new Uint8Array([1, 2, 3]);
  await tunnel.send(outbound);
  outbound.fill(0);
  assert.deepEqual(sent, [new Uint8Array([1, 2, 3])]);
  assert.deepEqual(await tunnel.receive(), new Uint8Array([4, 5, 6]));
  await tunnel.close();
  assert.equal(closed, true);
});

test("SandboxHandle rejects a PortSession without a single-use endpoint credential", async () => {
  let connected = false;
  const connector: PortTunnelConnector = {
    async connect() {
      connected = true;
      throw new Error("connector must not run");
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"), "lease-1");
  await assert.rejects(
    handle.connectPortTunnel(
      {
        id: "port-1",
        sandboxId: "sandbox-1",
        generation: 7,
        name: "ssh",
        protocol: "tcp",
        transport: "relay",
        endpoint: "wss://secondbox.example/v1/port-sessions/port-1",
        state: "open",
        createdAt: "2026-07-28T00:00:00Z",
        expiresAt: "2026-07-28T00:01:00Z",
      },
      connector,
    ),
    /endpoint credential is invalid/,
  );
  assert.equal(connected, false);
});

function sandbox(state: Sandbox["state"]): Sandbox {
  return {
    id: "sandbox-1",
    profile: "default",
    profileRevisionId: "profile-revision-1",
    state,
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

test("newIdempotencyKey returns distinct keys", () => {
  const keys = new Set(Array.from({ length: 32 }, () => newIdempotencyKey()));
  assert.equal(keys.size, 32);
});

test("revisionETag matches the service validator format", () => {
  assert.equal(revisionETag(7), '"revision-7"');
  assert.throws(() => revisionETag(0));
});

test("problemCodeOf reads typed failures only", () => {
  const problem = new SecondBoxProblemError(409, {
    type: "urn:secondbox:problem:conflict",
    title: "Sandbox generation is fenced",
    status: 409,
    code: "generation_fenced",
    requestId: "request-1",
    retryable: false,
  });
  assert.equal(problemCodeOf(problem), "generation_fenced");
  assert.equal(problemCodeOf(new Error("plain")), "");
});

test("decodeExecOutcome decodes output alongside a failing status", () => {
  const result = decodeExecOutcome({
    kind: "exited",
    exitCode: 23,
    elapsedMilliseconds: 5,
    output: {
      stdoutBase64: Buffer.from("out").toString("base64"),
      stderrBase64: Buffer.from("err").toString("base64"),
    },
  });
  assert.equal(result.kind, "exited");
  if (result.kind !== "exited") return;
  assert.equal(result.exitCode, 23);
  assert.equal(Buffer.from(result.stdout).toString(), "out");
  assert.equal(Buffer.from(result.stderr).toString(), "err");
});

test("createSandbox returns a handle for the created resource", async () => {
  let idempotency = "";
  const fetcher: typeof fetch = async (input, init) => {
    if (init?.method === "POST") {
      idempotency = new Headers(init.headers).get("Idempotency-Key") ?? "";
      return Response.json({
        id: "operation-1",
        sandboxId: "sandbox-1",
        kind: "create",
        state: "pending",
        requestId: "request-1",
        createdAt: "2026-07-28T00:00:00Z",
        updatedAt: "2026-07-28T00:00:00Z",
      });
    }
    return Response.json(sandbox("creating"));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const { handle, operation } = await api.createSandbox({ profile: "durable-coding" });
  assert.equal(operation.sandboxId, "sandbox-1");
  assert.equal(handle.snapshot.id, "sandbox-1");
  assert.notEqual(idempotency, "");
});

test("createSandbox rejects an operation without a Sandbox reference", async () => {
  const fetcher: typeof fetch = async () =>
    Response.json({
      id: "operation-1",
      sandboxId: "",
      kind: "create",
      state: "pending",
      requestId: "request-1",
      createdAt: "2026-07-28T00:00:00Z",
      updatedAt: "2026-07-28T00:00:00Z",
    });
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  await assert.rejects(
    api.createSandbox({ profile: "durable-coding" }),
    /no Sandbox reference/,
  );
});

test("waitFor retries after the service reports the wait expired", async () => {
  let waits = 0;
  const fetcher: typeof fetch = async (input) => {
    if (String(input).endsWith(":wait")) {
      waits += 1;
      if (waits === 1) {
        return Response.json({
          type: "urn:secondbox:problem:timeout",
          title: "Sandbox wait deadline expired",
          status: 408,
          code: "wait_expired",
          requestId: "request-1",
          retryable: true,
          details: [],
        }, { status: 408 });
      }
      return Response.json(sandbox("ready"));
    }
    return Response.json(sandbox("starting"));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("starting"));
  const result = await handle.waitFor(["ready"], { deadlineMilliseconds: 10_000 });
  assert.equal(result.state, "ready");
  assert.ok(waits >= 2);
});

test("waitFor keeps each service request below the default HTTP timeout", async () => {
  let requestDeadline = 0;
  const fetcher: typeof fetch = async (_input, init) => {
    const body = JSON.parse(String(init?.body)) as { deadlineMilliseconds: number };
    requestDeadline = body.deadlineMilliseconds;
    return Response.json(sandbox("ready"));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("starting"));
  await handle.waitFor(["ready"], { deadlineMilliseconds: 45_000 });
  assert.equal(requestDeadline, 20_000);
});

test("waitFor returns immediately when the state already holds", async () => {
  const fetcher: typeof fetch = async () => {
    throw new Error("a satisfied wait must not reach the service");
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("ready"));
  assert.equal((await handle.waitFor(["ready"], { deadlineMilliseconds: 1_000 })).state, "ready");
});

test("run creates, waits, and executes one command", async () => {
  const paths: string[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    paths.push(`${init?.method ?? "GET"} ${url.pathname}`);
    if (url.pathname === "/v1/sandboxes" && init?.method === "POST") {
      return Response.json({
        id: "operation-1",
        sandboxId: "sandbox-1",
        kind: "create",
        state: "pending",
        requestId: "request-1",
        createdAt: "2026-07-28T00:00:00Z",
        updatedAt: "2026-07-28T00:00:00Z",
      });
    }
    if (url.pathname.endsWith("/exec")) {
      return Response.json({
        kind: "exited",
        exitCode: 0,
        elapsedMilliseconds: 7,
        output: {
          stdoutBase64: Buffer.from("hello\n").toString("base64"),
          stderrBase64: "",
        },
      });
    }
    return Response.json(sandbox("ready"));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const outcome = await api.run({
    profile: "durable-coding",
    command: "echo hello",
    deadlineMilliseconds: 5_000,
    maximumOutputBytes: 1_048_576,
    readyTimeoutMilliseconds: 10_000,
  });
  assert.equal(outcome.sandbox.state, "ready");
  assert.equal(outcome.result.kind, "exited");
  if (outcome.result.kind !== "exited") return;
  assert.equal(Buffer.from(outcome.result.stdout).toString(), "hello\n");
  assert.ok(paths.includes("POST /v1/sandboxes"));
  assert.ok(paths.includes("POST /v1/sandboxes/sandbox-1/exec"));
  // run never deletes: disposal stays the caller's decision.
  assert.ok(!paths.some((entry) => entry.startsWith("DELETE")));
});

test("LeaseKeeper renews until closed and then releases", async () => {
  let renewals = 0;
  let releases = 0;
  const fetcher: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    if (init?.method === "DELETE") {
      releases += 1;
      return Response.json(lease(0));
    }
    if (url.pathname.endsWith(":renew")) renewals += 1;
    return Response.json(lease(20));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const keeper = new LeaseKeeper(api, lease(20), 60, 5);
  const deadline = Date.now() + 3_000;
  while (renewals < 2 && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  await keeper.close();
  assert.ok(renewals >= 2, `renewals = ${renewals}`);
  assert.equal(releases, 1);
  assert.equal(keeper.failure, undefined);
});

test("LeaseKeeper close reports the renewal failure rather than its consequence", async () => {
  const fetcher: typeof fetch = async (input, init) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith(":renew")) {
      return Response.json({
        type: "urn:secondbox:problem:conflict",
        title: "Lease is fenced",
        status: 409,
        code: "lease_fenced",
        requestId: "request-1",
        retryable: false,
        details: [],
      }, { status: 409 });
    }
    if (init?.method === "DELETE") {
      return Response.json({
        type: "urn:secondbox:problem:conflict",
        title: "Lease is inactive",
        status: 409,
        code: "lease_fenced",
        requestId: "request-2",
        retryable: false,
        details: [],
      }, { status: 409 });
    }
    return Response.json(lease(0));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const keeper = new LeaseKeeper(api, lease(0), 60, 5);
  const deadline = Date.now() + 3_000;
  while (keeper.failure === undefined && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  await assert.rejects(keeper.close(), /Lease renewal stopped/);
  assert.equal(problemCodeOf(keeper.failure), "lease_fenced");
});

function lease(expiresInMilliseconds: number): Lease {
  return {
    id: "lease-1",
    sandboxId: "sandbox-1",
    generation: 7,
    state: "active",
    expiresAt: new Date(Date.now() + expiresInMilliseconds).toISOString(),
    createdAt: "2026-07-28T00:00:00Z",
    updatedAt: "2026-07-28T00:00:00Z",
  };
}

test("SandboxHandle admits a direct PortSession through the framed handshake", async () => {
  const certificateSPKISHA256 = "a".repeat(64);
  const written: Uint8Array[] = [];
  const inbound: Uint8Array[] = [
    // The Runner may coalesce its verdict with the first payload bytes, so this
    // chunk carries both and the tunnel must replay only the payload.
    new Uint8Array([
      ...new TextEncoder().encode("SBXDP1"),
      0, 0, 0,
      4, 5, 6,
    ]),
  ];
  let closed = false;
  let dialed: { host: string; port: number; certificateSPKISHA256: string } | undefined;
  const dialer: DirectPortDialer = {
    async dial(descriptor) {
      dialed = {
        host: descriptor.host,
        port: descriptor.port,
        certificateSPKISHA256: descriptor.certificateSPKISHA256,
      };
      return {
        async write(payload) {
          written.push(payload.slice());
        },
        async read() {
          return inbound.shift() ?? new Uint8Array(0);
        },
        async close() {
          closed = true;
        },
      };
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"), "lease-1");
  const tunnel = await handle.connectPortTunnel(
    {
      id: "port-1",
      sandboxId: "sandbox-1",
      generation: 7,
      name: "ssh",
      protocol: "tcp",
      transport: "direct",
      endpoint: "secondbox+tcp://10.0.0.4:7443/v1/port-sessions/port-1#single-use-token",
      certificateSpkiSha256: certificateSPKISHA256,
      state: "open",
      createdAt: "2026-07-28T00:00:00Z",
      expiresAt: "2026-07-28T00:01:00Z",
    },
    { direct: dialer },
  );

  assert(tunnel instanceof PortTunnel);
  assert.deepEqual(dialed, {
    host: "10.0.0.4",
    port: 7443,
    certificateSPKISHA256,
  });
  assert.deepEqual(written, [
    new Uint8Array([
      ...new TextEncoder().encode("SBXDP1"),
      0, 0, 16,
      ...new TextEncoder().encode("single-use-token"),
    ]),
  ]);
  assert.deepEqual(await tunnel.receive(), new Uint8Array([4, 5, 6]));
  await tunnel.close();
  assert.equal(closed, true);
});

test("SandboxHandle surfaces a denied direct PortSession and closes the socket", async () => {
  const detail = "credential rejected";
  let closed = false;
  const dialer: DirectPortDialer = {
    async dial() {
      return {
        async write() {},
        async read() {
          return new Uint8Array([
            ...new TextEncoder().encode("SBXDP1"),
            1,
            0, detail.length,
            ...new TextEncoder().encode(detail),
          ]);
        },
        async close() {
          closed = true;
        },
      };
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"), "lease-1");
  await assert.rejects(
    handle.connectPortTunnel(
      {
        id: "port-1",
        sandboxId: "sandbox-1",
        generation: 7,
        name: "ssh",
        protocol: "tcp",
        transport: "direct",
        endpoint: "secondbox+tcp://10.0.0.4:7443/v1/port-sessions/port-1#single-use-token",
        certificateSpkiSha256: "a".repeat(64),
        state: "open",
        createdAt: "2026-07-28T00:00:00Z",
        expiresAt: "2026-07-28T00:01:00Z",
      },
      { direct: dialer },
    ),
    /denied: credential rejected/,
  );
  assert.equal(closed, true);
});

test("SandboxHandle refuses a direct PortSession when only a relay connector is supplied", async () => {
  let connected = false;
  const connector: PortTunnelConnector = {
    async connect() {
      connected = true;
      throw new Error("relay connector must not serve a direct session");
    },
  };
  const api = new SecondBox(
    new SecondBoxClient("https://secondbox.example", "token", async () => Response.json({})),
  );
  const handle = new SandboxHandle(api, sandbox("ready"), "lease-1");
  await assert.rejects(
    handle.connectPortTunnel(
      {
        id: "port-1",
        sandboxId: "sandbox-1",
        generation: 7,
        name: "ssh",
        protocol: "tcp",
        transport: "direct",
        endpoint: "secondbox+tcp://10.0.0.4:7443/v1/port-sessions/port-1#single-use-token",
        certificateSpkiSha256: "a".repeat(64),
        state: "open",
        createdAt: "2026-07-28T00:00:00Z",
        expiresAt: "2026-07-28T00:01:00Z",
      },
      connector,
    ),
    /direct transport has no dialer/,
  );
  assert.equal(connected, false);
});

test("high-level listing and adoption hide polling decoders and metadata query mechanics", async () => {
  let listedURL = "";
  const fetcher: typeof fetch = async (input) => {
    const url = new URL(String(input));
    if (url.pathname === "/v1/sandboxes") {
      listedURL = url.toString();
      return Response.json({ items: [] });
    }
    return Response.json(sandbox("ready"));
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  await api.listSandboxes({
    limit: 12,
    metadata: { z: "last", a: "first" },
  });
  const query = new URL(listedURL).searchParams;
  assert.equal(query.get("limit"), "12");
  assert.deepEqual(query.getAll("metadata"), ["a=first", "z=last"]);
  const handle = await api.adoptSandbox("sandbox-1");
  assert.equal(handle.snapshot.id, "sandbox-1");
});

test("high-level lifecycle and Metadata mutation use the observed revision fence", async () => {
  const ifMatches: string[] = [];
  const idempotencies: string[] = [];
  const fetcher: typeof fetch = async (input, init) => {
    ifMatches.push(new Headers(init?.headers).get("If-Match") ?? "");
    idempotencies.push(new Headers(init?.headers).get("Idempotency-Key") ?? "");
    if (new URL(String(input)).pathname.endsWith("/metadata")) {
      return Response.json({ ...sandbox("ready"), revision: 2, metadata: { owner: "application" } });
    }
    return Response.json({
      id: "operation-1", sandboxId: "sandbox-1", kind: "stop", state: "pending",
      requestId: "request-1", createdAt: "2026-08-03T00:00:00Z", updatedAt: "2026-08-03T00:00:00Z",
    });
  };
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  const handle = new SandboxHandle(api, sandbox("ready"));
  await handle.stop({});
  await handle.updateMetadata({ owner: "application" });
  assert.deepEqual(ifMatches, ['"revision-1"', '"revision-1"']);
  assert.notEqual(idempotencies[0], "");
  assert.equal(handle.snapshot.revision, 2);
});

test("high-level Artifact download requires its bound and Digest", async () => {
  const content = new TextEncoder().encode("artifact");
  const digest = await crypto.subtle.digest("SHA-256", content);
  const header = `sha-256=:${Buffer.from(digest).toString("base64")}:`;
  const fetcher: typeof fetch = async () => new Response(content, {
    headers: { Digest: header, "Content-Length": String(content.byteLength) },
  });
  const api = new SecondBox(new SecondBoxClient("https://secondbox.example", "token", fetcher));
  assert.deepEqual(await api.downloadArtifact("artifact-1", content.byteLength), content);
  await assert.rejects(api.downloadArtifact("artifact-1", content.byteLength - 1), /exceeds/);
});

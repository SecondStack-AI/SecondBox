import assert from "node:assert/strict";
import test from "node:test";

import {
  OperationFailedError,
  SandboxHandle,
  SecondBox,
  SecondBoxProblemError,
  type ExecStreamConnection,
  type ExecStreamConnector,
  type Sandbox,
  type TerminalConnection,
  type TerminalConnector,
} from "./client.ts";
import { SecondBoxClient } from "./transport.ts";

test("requestJSON uses hand-maintained operation metadata", async () => {
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
    `{"type":"outcome","sequence":1,"outcome":{"kind":"exited","exitCode":0,"output":{"stdoutBase64":"","stderrBase64":""}}}`,
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
    { type: "credit", sequence: 4, bytes: 4096 },
    { type: "resize", sequence: 5, rows: 40, columns: 120 },
    { type: "terminal_input", sequence: 6, dataBase64: "AAH+/w==" },
    { type: "cancel", sequence: 7 },
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

function sandbox(state: Sandbox["state"]): Sandbox {
  return {
    id: "sandbox-1",
    projectId: "project-1",
    profile: "default",
    profileRevisionId: "profile-revision-1",
    state,
    desiredState: "running",
    generation: 7,
    workspace: {
      id: "workspace-1",
      generation: 7,
      retainedBytes: 0,
      createdAt: "2026-07-28T00:00:00Z",
      updatedAt: "2026-07-28T00:00:00Z",
    },
    metadata: {},
    revision: 1,
    createdAt: "2026-07-28T00:00:00Z",
    updatedAt: "2026-07-28T00:00:00Z",
  };
}

import {
  OPERATIONS,
  SecondBoxAPIError,
  SecondBoxClient,
  encodeJSONBody,
  type CreateDirectoryRequest,
  type CreatePortSessionRequest,
  type CreateTerminalRequest,
  type DirectoryListing,
  type ExecOutcome,
  type ExecStreamFrame,
  type ExecStreamSession,
  type FileExistsResult,
  type FileStat,
  type FileWriteResult,
  type JSONValue,
  type Lease,
  type Metadata,
  type Operation,
  type OperationID,
  type PortSession,
  type Problem,
  type RestoreSnapshotRequest,
  type RemovePathRequest,
  type Sandbox,
  type SandboxPage,
  type SandboxState,
  type StreamingExecRequest,
  type TerminalFrame,
  type TerminalSession,
  type TransportRequestOptions,
  type UpdateSandboxMetadataRequest,
  type WaitSandboxRequest,
} from "./transport.ts";

export type {
  CreateAPIKeyResponse,
  ExecStreamFrame,
  FileStat,
  Lease,
  Metadata,
  Operation,
  PortSession,
  Profile,
  ProfileRevisionSpec,
  Problem,
  Project,
  Sandbox,
  SandboxPage,
  SandboxState,
  Snapshot,
  ServiceAccount,
  ServiceAccountScope,
  TerminalFrame,
  UpdateSandboxMetadataRequest,
} from "./transport.ts";
export { SecondBoxClient, encodeJSONBody } from "./transport.ts";

/** A decoded non-successful SecondBox response. */
export class SecondBoxProblemError extends Error {
  public readonly status: number;
  public readonly problem: Problem;

  public constructor(status: number, problem: Problem) {
    super(
      `SecondBox API request failed: status=${status} code=${problem.code} title=${problem.title}`,
    );
    this.name = "SecondBoxProblemError";
    this.status = status;
    this.problem = problem;
  }
}

/** A terminal asynchronous operation that did not succeed. */
export class OperationFailedError extends Error {
  public readonly operation: Operation;

  public constructor(operation: Operation) {
    const detail =
      operation.error === undefined
        ? ""
        : ` code=${operation.error.code} title=${operation.error.title}`;
    super(`SecondBox operation failed: operation=${operation.id} state=${operation.state}${detail}`);
    this.name = "OperationFailedError";
    this.operation = operation;
  }
}

export interface PollOptions {
  readonly intervalMilliseconds: number;
  readonly signal?: AbortSignal;
}

/** Handwritten typed helpers over the generated dependency-free transport. */
export class SecondBox {
  public readonly transport: SecondBoxClient;

  public constructor(transport: SecondBoxClient) {
    this.transport = transport;
  }

  public async request(
    operationID: OperationID,
    options: TransportRequestOptions = {},
  ): Promise<Response> {
    try {
      return await this.transport.send(OPERATIONS[operationID], options);
    } catch (error) {
      if (!(error instanceof SecondBoxAPIError)) throw error;
      const problem = (await error.response.json()) as Problem;
      throw new SecondBoxProblemError(error.response.status, problem);
    }
  }

  public async requestJSON<T>(
    operationID: OperationID,
    options: TransportRequestOptions = {},
  ): Promise<T> {
    const response = await this.request(operationID, options);
    return (await response.json()) as T;
  }

  public async requestVoid(
    operationID: OperationID,
    options: TransportRequestOptions = {},
  ): Promise<void> {
    await this.request(operationID, options);
  }

  public async waitOperation(
    operationID: string,
    options: PollOptions,
  ): Promise<Operation> {
    if (!Number.isFinite(options.intervalMilliseconds) || options.intervalMilliseconds <= 0) {
      throw new Error("SecondBox operation polling interval must be positive");
    }
    for (;;) {
      throwIfAborted(options.signal);
      const operation = await this.requestJSON<Operation>("getOperation", {
        pathParameters: { operationId: operationID },
        signal: options.signal,
      });
      switch (operation.state) {
        case "succeeded":
          return operation;
        case "failed":
        case "cancelled":
          throw new OperationFailedError(operation);
        case "pending":
        case "running":
          await abortableDelay(options.intervalMilliseconds, options.signal);
          break;
        default:
          throw new Error(
            `SecondBox operation ${operation.id} has unknown state ${String(operation.state)}`,
          );
      }
    }
  }

  public sandbox(snapshot: Sandbox, leaseID?: string): SandboxHandle {
    return new SandboxHandle(this, snapshot, leaseID);
  }

  public getLease(leaseID: string, signal?: AbortSignal): Promise<Lease> {
    requireNonempty(leaseID, "Lease ID");
    return this.requestJSON<Lease>("getSandboxLease", {
      pathParameters: { leaseId: leaseID },
      signal,
    });
  }

  public renewLease(
    leaseID: string,
    durationSeconds: number,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<Lease> {
    requireNonempty(leaseID, "Lease ID");
    requireDurationSeconds(durationSeconds, "Lease");
    requireNonempty(idempotency, "Lease renewal idempotency key");
    return this.requestJSON<Lease>("renewSandboxLease", {
      pathParameters: { leaseId: leaseID },
      headers: { "Idempotency-Key": idempotency },
      body: encodeJSONBody({ durationSeconds }),
      signal,
    });
  }

  public releaseLease(
    leaseID: string,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<Lease> {
    requireNonempty(leaseID, "Lease ID");
    requireNonempty(idempotency, "Lease release idempotency key");
    return this.requestJSON<Lease>("releaseSandboxLease", {
      pathParameters: { leaseId: leaseID },
      headers: { "Idempotency-Key": idempotency },
      signal,
    });
  }
}

export interface ExecOptions {
  readonly cwd?: string;
  readonly environment: Readonly<Record<string, string>>;
  readonly deadlineMilliseconds: number;
  readonly maximumOutputBytes: number;
  readonly signal?: AbortSignal;
}

export type ExecResult =
  | {
      readonly kind: "exited";
      readonly exitCode: number;
      readonly signal?: number;
      readonly elapsedMilliseconds: number;
      readonly stdout: Uint8Array;
      readonly stderr: Uint8Array;
    }
  | {
      readonly kind: "deadline_exceeded";
      readonly elapsedMilliseconds: number;
      readonly stdout: Uint8Array;
      readonly stderr: Uint8Array;
    }
  | {
      readonly kind: "cancelled";
      readonly stdout: Uint8Array;
      readonly stderr: Uint8Array;
    }
  | {
      readonly kind: "output_exhausted";
      readonly limitBytes: number;
      readonly stdout: Uint8Array;
      readonly stderr: Uint8Array;
    }
  | {
      readonly kind: "spawn_failed";
      readonly reason: string;
      readonly message: string;
    }
  | {
      readonly kind: "infrastructure_failed";
      readonly reason: string;
      readonly message: string;
      readonly retryable: boolean;
    };

/** An authenticated text-frame connection supplied by the application runtime. */
export interface ExecStreamConnection {
  readonly subprotocol: string;
  sendText(payload: string): Promise<void>;
  receiveText(signal?: AbortSignal): Promise<string>;
  close(): Promise<void>;
}

/** Injects the runtime-specific authenticated WebSocket implementation. */
export interface ExecStreamConnector {
  connect(
    descriptor: {
      readonly websocketURL: string;
      readonly subprotocol: "secondbox.exec.v1";
      readonly sandboxID: string;
      readonly generation: number;
      readonly expiresAt: string;
    },
    signal?: AbortSignal,
  ): Promise<ExecStreamConnection>;
}

/** Ordered streaming-exec helper over an injected authenticated connection. */
export class ExecStream {
  readonly #connection: ExecStreamConnection;
  #nextClientSequence = 0;
  #nextServerSequence = 0;
  #inputClosed = false;
  #terminal = false;
  #writeTail: Promise<void> = Promise.resolve();
  #readTail: Promise<void> = Promise.resolve();

  public constructor(connection: ExecStreamConnection) {
    if (connection.subprotocol !== "secondbox.exec.v1") {
      throw new Error("SecondBox Exec stream subprotocol was not negotiated");
    }
    this.#connection = connection;
  }

  public sendInput(data: Uint8Array): Promise<void> {
    return this.sendInputFrame(data, false);
  }

  public closeInput(): Promise<void> {
    return this.sendInputFrame(new Uint8Array(), true);
  }

  public sendInputFrame(data: Uint8Array, endOfInput: boolean): Promise<void> {
    const owned = new Uint8Array(data);
    if (owned.byteLength === 0 && !endOfInput) {
      return Promise.reject(new Error("SecondBox Exec stream stdin frame is empty"));
    }
    return this.enqueueWrite(async () => {
      if (this.#inputClosed) {
        throw new Error("SecondBox Exec stream standard input is already closed");
      }
      await this.sendFrame({
        type: "stdin",
        sequence: this.#nextClientSequence,
        dataBase64: encodeBase64(owned),
        endOfInput,
      });
      if (endOfInput) this.#inputClosed = true;
    });
  }

  public grantOutput(bytes: number): Promise<void> {
    requirePositiveInteger(bytes, "Exec stream output credit");
    return this.enqueueWrite(() =>
      this.sendFrame({
        type: "credit",
        sequence: this.#nextClientSequence,
        bytes,
      }),
    );
  }

  public cancel(): Promise<void> {
    return this.enqueueWrite(() =>
      this.sendFrame({
        type: "cancel",
        sequence: this.#nextClientSequence,
      }),
    );
  }

  public receive(signal?: AbortSignal): Promise<ExecStreamFrame> {
    const result = this.#readTail.then(async () => {
      if (this.#terminal) {
        throw new Error("SecondBox Exec stream is terminal");
      }
      const payload = await this.#connection.receiveText(signal);
      const frame = decodeExecServerFrame(payload, this.#nextServerSequence);
      this.#nextServerSequence++;
      if (frame.type === "outcome") this.#terminal = true;
      return frame;
    });
    this.#readTail = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  public close(): Promise<void> {
    return this.#connection.close();
  }

  private enqueueWrite(write: () => Promise<void>): Promise<void> {
    const result = this.#writeTail.then(async () => {
      if (this.#terminal) {
        throw new Error("SecondBox Exec stream is terminal");
      }
      await write();
    });
    this.#writeTail = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  private async sendFrame(frame: ExecStreamFrame): Promise<void> {
    await this.#connection.sendText(JSON.stringify(frame));
    this.#nextClientSequence++;
  }
}

/** An authenticated text-frame connection supplied by the application runtime. */
export interface TerminalConnection {
  readonly subprotocol: string;
  sendText(payload: string): Promise<void>;
  receiveText(signal?: AbortSignal): Promise<string>;
  close(): Promise<void>;
}

/** Injects the runtime-specific authenticated Terminal WebSocket implementation. */
export interface TerminalConnector {
  connect(
    descriptor: {
      readonly websocketURL: string;
      readonly subprotocol: "secondbox.terminal.v1";
      readonly sandboxID: string;
      readonly generation: number;
      readonly expiresAt: string;
    },
    signal?: AbortSignal,
  ): Promise<TerminalConnection>;
}

/** Ordered binary-safe helper for one Terminal WebSocket attachment. */
export class Terminal {
  readonly #connection: TerminalConnection;
  #nextClientSequence: number;
  #nextServerSequence = 0;
  #terminal = false;
  #writeTail: Promise<void> = Promise.resolve();
  #readTail: Promise<void> = Promise.resolve();

  public constructor(connection: TerminalConnection, nextClientSequence: number) {
    if (connection.subprotocol !== "secondbox.terminal.v1") {
      throw new Error("SecondBox Terminal subprotocol was not negotiated");
    }
    if (!Number.isSafeInteger(nextClientSequence) || nextClientSequence < 0) {
      throw new Error("SecondBox Terminal next client sequence is invalid");
    }
    this.#connection = connection;
    this.#nextClientSequence = nextClientSequence;
  }

  public sendInput(data: Uint8Array): Promise<void> {
    const owned = new Uint8Array(data);
    if (owned.byteLength === 0) {
      return Promise.reject(new Error("SecondBox Terminal input frame is empty"));
    }
    return this.enqueueWrite(() =>
      this.sendFrame({
        type: "terminal_input",
        sequence: this.#nextClientSequence,
        dataBase64: encodeBase64(owned),
      }),
    );
  }

  public resize(rows: number, columns: number): Promise<void> {
    requirePositiveInteger(rows, "Terminal rows");
    requirePositiveInteger(columns, "Terminal columns");
    if (rows > 1000 || columns > 1000) {
      return Promise.reject(new Error("SecondBox Terminal resize dimensions are invalid"));
    }
    return this.enqueueWrite(() =>
      this.sendFrame({
        type: "resize",
        sequence: this.#nextClientSequence,
        rows,
        columns,
      }),
    );
  }

  public grantOutput(bytes: number): Promise<void> {
    requirePositiveInteger(bytes, "Terminal output credit");
    return this.enqueueWrite(() =>
      this.sendFrame({
        type: "credit",
        sequence: this.#nextClientSequence,
        bytes,
      }),
    );
  }

  public cancel(): Promise<void> {
    return this.enqueueWrite(() =>
      this.sendFrame({
        type: "cancel",
        sequence: this.#nextClientSequence,
      }),
    );
  }

  public receive(signal?: AbortSignal): Promise<TerminalFrame> {
    const result = this.#readTail.then(async () => {
      if (this.#terminal) {
        throw new Error("SecondBox Terminal is terminal");
      }
      const payload = await this.#connection.receiveText(signal);
      const frame = decodeTerminalServerFrame(payload, this.#nextServerSequence);
      this.#nextServerSequence++;
      if (frame.type === "outcome") this.#terminal = true;
      return frame;
    });
    this.#readTail = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  public close(): Promise<void> {
    return this.#connection.close();
  }

  private enqueueWrite(write: () => Promise<void>): Promise<void> {
    const result = this.#writeTail.then(async () => {
      if (this.#terminal) {
        throw new Error("SecondBox Terminal is terminal");
      }
      await write();
    });
    this.#writeTail = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  private async sendFrame(frame: TerminalFrame): Promise<void> {
    await this.#connection.sendText(JSON.stringify(frame));
    this.#nextClientSequence++;
  }
}

/** An authenticated binary-frame connection supplied by the application runtime. */
export interface PortTunnelConnection {
  readonly subprotocol: string;
  sendBinary(payload: Uint8Array): Promise<void>;
  receiveBinary(signal?: AbortSignal): Promise<Uint8Array>;
  close(): Promise<void>;
}

/** Injects the runtime-specific authenticated Port WebSocket implementation. */
export interface PortTunnelConnector {
  connect(
    descriptor: {
      readonly websocketURL: string;
      readonly subprotocols: readonly [
        "secondbox.port.v1",
        `secondbox.port.token.${string}`,
      ];
      readonly sandboxID: string;
      readonly generation: number;
      readonly expiresAt: string;
    },
    signal?: AbortSignal,
  ): Promise<PortTunnelConnection>;
}

/** Binary stream carried by one generation-fenced authenticated PortSession. */
export class PortTunnel {
  readonly #connection: PortTunnelConnection;
  #writeTail: Promise<void> = Promise.resolve();
  #readTail: Promise<void> = Promise.resolve();

  public constructor(connection: PortTunnelConnection) {
    if (connection.subprotocol !== "secondbox.port.v1") {
      throw new Error("SecondBox Port tunnel subprotocol was not negotiated");
    }
    this.#connection = connection;
  }

  public send(payload: Uint8Array): Promise<void> {
    if (payload.byteLength === 0) {
      return Promise.reject(new Error("SecondBox Port tunnel frame is empty"));
    }
    const owned = new Uint8Array(payload);
    const result = this.#writeTail.then(() => this.#connection.sendBinary(owned));
    this.#writeTail = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  public receive(signal?: AbortSignal): Promise<Uint8Array> {
    const result = this.#readTail.then(async () => {
      const payload = await this.#connection.receiveBinary(signal);
      if (payload.byteLength === 0) {
        throw new Error("SecondBox Port tunnel received an empty frame");
      }
      return new Uint8Array(payload);
    });
    this.#readTail = result.then(
      () => undefined,
      () => undefined,
    );
    return result;
  }

  public close(): Promise<void> {
    return this.#connection.close();
  }
}

/** Filesystem and command surface consumed by the Flue adapter. */
export interface SandboxFilesystem {
  readFile(path: string, signal?: AbortSignal): Promise<Uint8Array>;
  writeFile(path: string, content: Uint8Array, signal?: AbortSignal): Promise<unknown>;
  statFile(path: string, signal?: AbortSignal): Promise<Pick<FileStat, "kind" | "sizeBytes" | "modifiedAt">>;
  listDirectory(path: string, signal?: AbortSignal): Promise<readonly Pick<FileStat, "path">[]>;
  fileExists(path: string, signal?: AbortSignal): Promise<boolean>;
  createDirectory(path: string, recursive: boolean, signal?: AbortSignal): Promise<void>;
  removePath(path: string, recursive: boolean, force: boolean, signal?: AbortSignal): Promise<void>;
  exec(command: string, options: ExecOptions): Promise<ExecResult>;
}

export interface LifecycleOptions {
  readonly idempotencyKey: string;
  readonly ifMatch: string;
  readonly signal?: AbortSignal;
}

/** Caller-owned durable Sandbox handle; no method runs implicitly on close or disconnect. */
export class SandboxHandle implements SandboxFilesystem {
  readonly #api: SecondBox;
  readonly #leaseID: string | undefined;
  #snapshot: Sandbox;

  public constructor(api: SecondBox, snapshot: Sandbox, leaseID?: string) {
    this.#api = api;
    this.#snapshot = snapshot;
    this.#leaseID = leaseID;
  }

  public get snapshot(): Sandbox {
    return this.#snapshot;
  }

  public async refresh(signal?: AbortSignal): Promise<Sandbox> {
    const sandbox = await this.#api.requestJSON<Sandbox>("getSandbox", {
      pathParameters: { sandboxId: this.#snapshot.id },
      signal,
    });
    this.#snapshot = sandbox;
    return sandbox;
  }

  public async wait(
    states: readonly SandboxState[],
    deadlineMilliseconds: number,
    signal?: AbortSignal,
  ): Promise<Sandbox> {
    if (states.length === 0) {
      throw new Error("SecondBox Sandbox wait requires at least one state");
    }
    requirePositiveInteger(deadlineMilliseconds, "Sandbox wait deadlineMilliseconds");
    if (deadlineMilliseconds > 60_000) {
      throw new Error("SecondBox Sandbox wait deadlineMilliseconds must not exceed 60000");
    }
    const body: WaitSandboxRequest = { states, deadlineMilliseconds };
    const sandbox = await this.#api.requestJSON<Sandbox>("waitForSandbox", {
      pathParameters: { sandboxId: this.#snapshot.id },
      body: encodeJSONBody(body),
      signal,
    });
    this.#snapshot = sandbox;
    return sandbox;
  }

  public start(options: LifecycleOptions): Promise<Operation> {
    return this.lifecycle("startSandbox", options);
  }

  public drain(options: LifecycleOptions): Promise<Operation> {
    return this.lifecycle("drainSandbox", options);
  }

  public stop(options: LifecycleOptions): Promise<Operation> {
    return this.lifecycle("stopSandbox", options);
  }

  public restore(
    snapshotId: string,
    options: LifecycleOptions,
  ): Promise<Operation> {
    const body: RestoreSnapshotRequest = { snapshotId };
    return this.lifecycle("restoreSandboxSnapshot", options, body as unknown as JSONValue);
  }

  public delete(options: LifecycleOptions): Promise<Operation> {
    return this.lifecycle("deleteSandbox", options);
  }

  public async exec(command: string, options: ExecOptions): Promise<ExecResult> {
    requirePositiveInteger(options.deadlineMilliseconds, "exec deadlineMilliseconds");
    requirePositiveInteger(options.maximumOutputBytes, "exec maximumOutputBytes");
    const outcome = await this.#api.requestJSON<ExecOutcome>("executeSandboxCommand", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        ...this.dataPlaneHeaders(),
        "Idempotency-Key": idempotencyKey(),
      },
      body: encodeJSONBody({
        command: { mode: "shell", command },
        ...(options.cwd === undefined ? {} : { cwd: options.cwd }),
        environment: options.environment,
        deadlineMilliseconds: options.deadlineMilliseconds,
        maximumOutputBytes: options.maximumOutputBytes,
      }),
      signal: options.signal,
    });
    return decodeExecOutcome(outcome);
  }

  /** Acquires one explicit generation-bound Lease for active data-plane work. */
  public acquireLease(
    durationSeconds: number,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<Lease> {
    requireDurationSeconds(durationSeconds, "Lease");
    requireNonempty(idempotency, "Lease acquisition idempotency key");
    return this.#api.requestJSON<Lease>("acquireSandboxLease", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        "SecondBox-Generation": String(this.#snapshot.generation),
        "Idempotency-Key": idempotency,
      },
      body: encodeJSONBody({ durationSeconds }),
      signal,
    });
  }

  /** Negotiates a streaming-exec session while leaving WebSocket ownership to the caller. */
  public createExecStream(
    request: StreamingExecRequest,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<ExecStreamSession> {
    return this.negotiateDataPlaneSession(
      "createSandboxExecStream",
      request as unknown as JSONValue,
      idempotency,
      signal,
    );
  }

  /** Attaches an authenticated connector and validates the session fence. */
  public async connectExecStream(
    session: ExecStreamSession,
    connector: ExecStreamConnector,
    signal?: AbortSignal,
  ): Promise<ExecStream> {
    if (
      session.sandboxId !== this.#snapshot.id ||
      session.generation !== this.#snapshot.generation ||
      session.state !== "open" ||
      session.subprotocol !== "secondbox.exec.v1"
    ) {
      throw new Error("SecondBox Exec stream session does not match the Sandbox handle");
    }
    const endpoint = new URL(session.websocketUrl);
    if (!Number.isFinite(Date.parse(session.expiresAt))) {
      throw new Error("SecondBox Exec stream expiration is invalid");
    }
    if (
      (endpoint.protocol !== "ws:" && endpoint.protocol !== "wss:") ||
      endpoint.username !== "" ||
      endpoint.password !== "" ||
      endpoint.search !== "" ||
      endpoint.hash !== ""
    ) {
      throw new Error("SecondBox Exec stream WebSocket URL is invalid");
    }
    const connection = await connector.connect(
      {
        websocketURL: endpoint.toString(),
        subprotocol: session.subprotocol,
        sandboxID: session.sandboxId,
        generation: session.generation,
        expiresAt: session.expiresAt,
      },
      signal,
    );
    return new ExecStream(connection);
  }

  /** Negotiates a terminal session while leaving WebSocket ownership to the caller. */
  public createTerminal(
    request: CreateTerminalRequest,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<TerminalSession> {
    return this.negotiateDataPlaneSession(
      "createSandboxTerminal",
      request as unknown as JSONValue,
      idempotency,
      signal,
    );
  }

  /** Returns the current reconnect descriptor for one stable Terminal ID. */
  public getTerminal(
    terminalSessionID: string,
    signal?: AbortSignal,
  ): Promise<TerminalSession> {
    if (terminalSessionID === "") {
      return Promise.reject(new Error("SecondBox Terminal session ID is required"));
    }
    return this.#api.requestJSON<TerminalSession>("reconnectSandboxTerminal", {
      pathParameters: {
        sandboxId: this.#snapshot.id,
        terminalSessionId: terminalSessionID,
      },
      headers: this.dataPlaneHeaders(),
      signal,
    });
  }

  /** Durably requests cancellation and returns the observable Terminal descriptor. */
  public cancelTerminal(
    terminalSessionID: string,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<TerminalSession> {
    if (terminalSessionID === "" || idempotency === "") {
      return Promise.reject(
        new Error("SecondBox Terminal session ID and cancellation idempotency are required"),
      );
    }
    return this.#api.requestJSON<TerminalSession>("cancelSandboxTerminal", {
      pathParameters: {
        sandboxId: this.#snapshot.id,
        terminalSessionId: terminalSessionID,
      },
      headers: {
        ...this.dataPlaneHeaders(),
        "Idempotency-Key": idempotency,
      },
      signal,
    });
  }

  /** Attaches an authenticated Terminal connector and validates the session fence. */
  public async connectTerminal(
    session: TerminalSession,
    connector: TerminalConnector,
    signal?: AbortSignal,
  ): Promise<Terminal> {
    if (
      session.sandboxId !== this.#snapshot.id ||
      session.generation !== this.#snapshot.generation ||
      (session.state !== "open" && session.state !== "detached") ||
      session.subprotocol !== "secondbox.terminal.v1"
    ) {
      throw new Error("SecondBox Terminal session does not match the Sandbox handle");
    }
    const endpoint = new URL(session.websocketUrl);
    if (!Number.isFinite(Date.parse(session.expiresAt))) {
      throw new Error("SecondBox Terminal expiration is invalid");
    }
    if (
      (endpoint.protocol !== "ws:" && endpoint.protocol !== "wss:") ||
      endpoint.username !== "" ||
      endpoint.password !== "" ||
      endpoint.search !== "" ||
      endpoint.hash !== ""
    ) {
      throw new Error("SecondBox Terminal WebSocket URL is invalid");
    }
    const connection = await connector.connect(
      {
        websocketURL: endpoint.toString(),
        subprotocol: session.subprotocol,
        sandboxID: session.sandboxId,
        generation: session.generation,
        expiresAt: session.expiresAt,
      },
      signal,
    );
    return new Terminal(connection, session.nextClientSequence);
  }

  /** Creates one generation- and Lease-fenced authenticated PortSession. */
  public createPortSession(
    request: CreatePortSessionRequest,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<PortSession> {
    requireNonempty(request.name, "PortSession name");
    requireDurationSeconds(request.durationSeconds, "PortSession");
    return this.negotiateDataPlaneSession(
      "createSandboxPortSession",
      request as unknown as JSONValue,
      idempotency,
      signal,
    );
  }

  /** Returns the observable state of one PortSession without consuming its credential. */
  public getPortSession(
    portSessionID: string,
    signal?: AbortSignal,
  ): Promise<PortSession> {
    if (portSessionID === "") {
      return Promise.reject(new Error("SecondBox PortSession ID is required"));
    }
    return this.#api.requestJSON<PortSession>("getSandboxPortSession", {
      pathParameters: {
        sandboxId: this.#snapshot.id,
        portSessionId: portSessionID,
      },
      signal,
    });
  }

  /** Explicitly closes one PortSession. */
  public async closePortSession(
    portSessionID: string,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<void> {
    if (portSessionID === "" || idempotency === "") {
      throw new Error(
        "SecondBox PortSession ID and close idempotency are required",
      );
    }
    await this.#api.requestVoid("closeSandboxPortSession", {
      pathParameters: {
        sandboxId: this.#snapshot.id,
        portSessionId: portSessionID,
      },
      headers: { "Idempotency-Key": idempotency },
      signal,
    });
  }

  /** Consumes the endpoint credential through an authenticated binary connector. */
  public async connectPortTunnel(
    session: PortSession,
    connector: PortTunnelConnector,
    signal?: AbortSignal,
  ): Promise<PortTunnel> {
    if (
      session.sandboxId !== this.#snapshot.id ||
      session.generation !== this.#snapshot.generation ||
      session.state !== "open"
    ) {
      throw new Error("SecondBox PortSession does not match the Sandbox handle");
    }
    const endpoint = new URL(session.endpoint);
    const credential = endpoint.hash.slice(1);
    if (!Number.isFinite(Date.parse(session.expiresAt))) {
      throw new Error("SecondBox PortSession expiration is invalid");
    }
    if (
      (endpoint.protocol !== "ws:" && endpoint.protocol !== "wss:") ||
      endpoint.username !== "" ||
      endpoint.password !== "" ||
      endpoint.search !== ""
    ) {
      throw new Error("SecondBox PortSession WebSocket URL is invalid");
    }
    if (
      credential === "" ||
      credential.length > 2048 ||
      !/^[A-Za-z0-9_.-]+$/.test(credential)
    ) {
      throw new Error("SecondBox PortSession endpoint credential is invalid");
    }
    endpoint.hash = "";
    const connection = await connector.connect(
      {
        websocketURL: endpoint.toString(),
        subprotocols: [
          "secondbox.port.v1",
          `secondbox.port.token.${credential}`,
        ],
        sandboxID: session.sandboxId,
        generation: session.generation,
        expiresAt: session.expiresAt,
      },
      signal,
    );
    return new PortTunnel(connection);
  }

  public async readFile(path: string, signal?: AbortSignal): Promise<Uint8Array> {
    const response = await this.#api.request("readSandboxFile", {
      pathParameters: { sandboxId: this.#snapshot.id },
      queryParameters: { path },
      headers: this.dataPlaneHeaders(),
      signal,
    });
    return new Uint8Array(await response.arrayBuffer());
  }

  public async writeFile(
    path: string,
    content: Uint8Array,
    signal?: AbortSignal,
  ): Promise<FileWriteResult> {
    const digest = await sha256Digest(content);
    return this.#api.requestJSON<FileWriteResult>("writeSandboxFile", {
      pathParameters: { sandboxId: this.#snapshot.id },
      queryParameters: { path },
      headers: {
        ...this.dataPlaneHeaders(),
        Digest: digest,
        "Idempotency-Key": idempotencyKey(),
      },
      body: ownedArrayBuffer(content),
      contentType: "application/octet-stream",
      signal,
    });
  }

  public statFile(path: string, signal?: AbortSignal): Promise<FileStat> {
    return this.#api.requestJSON<FileStat>("statSandboxFile", {
      pathParameters: { sandboxId: this.#snapshot.id },
      queryParameters: { path },
      headers: this.dataPlaneHeaders(),
      signal,
    });
  }

  public async listDirectory(path: string, signal?: AbortSignal): Promise<readonly FileStat[]> {
    const listing = await this.#api.requestJSON<DirectoryListing>("listSandboxDirectory", {
      pathParameters: { sandboxId: this.#snapshot.id },
      queryParameters: { path },
      headers: this.dataPlaneHeaders(),
      signal,
    });
    return listing.entries;
  }

  public async fileExists(path: string, signal?: AbortSignal): Promise<boolean> {
    const result = await this.#api.requestJSON<FileExistsResult>("sandboxFileExists", {
      pathParameters: { sandboxId: this.#snapshot.id },
      queryParameters: { path },
      headers: this.dataPlaneHeaders(),
      signal,
    });
    return result.exists;
  }

  public async createDirectory(
    path: string,
    recursive: boolean,
    signal?: AbortSignal,
  ): Promise<void> {
    const body: CreateDirectoryRequest = { path, recursive };
    await this.#api.requestVoid("createSandboxDirectory", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        ...this.dataPlaneHeaders(),
        "Idempotency-Key": idempotencyKey(),
      },
      body: encodeJSONBody(body),
      signal,
    });
  }

  public async removePath(
    path: string,
    recursive: boolean,
    force: boolean,
    signal?: AbortSignal,
  ): Promise<void> {
    const body: RemovePathRequest = { path, recursive, force };
    await this.#api.requestVoid("removeSandboxPath", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        ...this.dataPlaneHeaders(),
        "Idempotency-Key": idempotencyKey(),
      },
      body: encodeJSONBody(body),
      signal,
    });
  }

  private async lifecycle(
    operationID:
      | "startSandbox"
      | "drainSandbox"
      | "stopSandbox"
      | "restoreSandboxSnapshot"
      | "deleteSandbox",
    options: LifecycleOptions,
    body?: JSONValue,
  ): Promise<Operation> {
    if (options.idempotencyKey === "") {
      throw new Error(`SecondBox ${operationID} idempotency key is required`);
    }
    if (options.ifMatch === "") {
      throw new Error(`SecondBox ${operationID} If-Match value is required`);
    }
    const operation = await this.#api.requestJSON<Operation>(operationID, {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        "Idempotency-Key": options.idempotencyKey,
        "If-Match": options.ifMatch,
      },
      ...(body === undefined ? {} : { body: encodeJSONBody(body) }),
      signal: options.signal,
    });
    if (operation.sandbox !== undefined) this.#snapshot = operation.sandbox;
    return operation;
  }

  private dataPlaneHeaders(): Readonly<Record<string, string>> {
    return {
      "SecondBox-Generation": String(this.#snapshot.generation),
      ...(this.#leaseID === undefined ? {} : { "SecondBox-Lease-ID": this.#leaseID }),
    };
  }

  private negotiateDataPlaneSession<T>(
    operationID:
      | "createSandboxExecStream"
      | "createSandboxTerminal"
      | "createSandboxPortSession",
    request: JSONValue,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<T> {
    if (idempotency === "") {
      throw new Error(`SecondBox ${operationID} idempotency key is required`);
    }
    return this.#api.requestJSON<T>(operationID, {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        ...this.dataPlaneHeaders(),
        "Idempotency-Key": idempotency,
      },
      body: encodeJSONBody(request),
      signal,
    });
  }
}

function decodeExecOutcome(outcome: ExecOutcome): ExecResult {
  switch (outcome.kind) {
    case "exited":
      return {
        kind: outcome.kind,
        exitCode: outcome.exitCode,
        ...(outcome.signal === undefined ? {} : { signal: outcome.signal }),
        elapsedMilliseconds: outcome.elapsedMilliseconds,
        ...decodeOutput(outcome.output),
      };
    case "deadline_exceeded":
      return {
        kind: outcome.kind,
        elapsedMilliseconds: outcome.elapsedMilliseconds,
        ...decodeOutput(outcome.output),
      };
    case "cancelled":
      return { kind: outcome.kind, ...decodeOutput(outcome.output) };
    case "output_exhausted":
      return {
        kind: outcome.kind,
        limitBytes: outcome.limitBytes,
        ...decodeOutput(outcome.output),
      };
    case "spawn_failed":
      return {
        kind: outcome.kind,
        reason: outcome.reason,
        message: outcome.message,
      };
    case "infrastructure_failed":
      return {
        kind: outcome.kind,
        reason: outcome.reason,
        message: outcome.message,
        retryable: outcome.retryable,
      };
  }
}

function decodeOutput(output: {
  readonly stdoutBase64: string;
  readonly stderrBase64: string;
}): { stdout: Uint8Array; stderr: Uint8Array } {
  return {
    stdout: decodeBase64(output.stdoutBase64),
    stderr: decodeBase64(output.stderrBase64),
  };
}

function decodeBase64(value: string): Uint8Array {
  const decoded = atob(value);
  const bytes = new Uint8Array(decoded.length);
  for (let index = 0; index < decoded.length; index++) {
    bytes[index] = decoded.charCodeAt(index);
  }
  return bytes;
}

function encodeBase64(value: Uint8Array): string {
  let binary = "";
  for (let offset = 0; offset < value.byteLength; offset += 8192) {
    const chunk = value.subarray(offset, Math.min(offset + 8192, value.byteLength));
    for (const byte of chunk) binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}

function decodeExecServerFrame(payload: string, expectedSequence: number): ExecStreamFrame {
  let decoded: unknown;
  try {
    decoded = JSON.parse(payload);
  } catch {
    throw new Error("SecondBox Exec stream received invalid JSON");
  }
  if (
    typeof decoded !== "object" ||
    decoded === null ||
    !("type" in decoded) ||
    !("sequence" in decoded) ||
    !Number.isSafeInteger(decoded.sequence) ||
    decoded.sequence !== expectedSequence
  ) {
    throw new Error("SecondBox Exec stream server sequence is invalid");
  }
  if (decoded.type === "output") {
    if (
      !("stream" in decoded) ||
      (decoded.stream !== "stdout" && decoded.stream !== "stderr") ||
      !("dataBase64" in decoded) ||
      typeof decoded.dataBase64 !== "string"
    ) {
      throw new Error("SecondBox Exec stream output frame is invalid");
    }
    const bytes = decodeBase64(decoded.dataBase64);
    if (encodeBase64(bytes) !== decoded.dataBase64) {
      throw new Error("SecondBox Exec stream output is not canonical base64");
    }
    return decoded as ExecStreamFrame;
  }
  if (
    decoded.type === "outcome" &&
    "outcome" in decoded &&
    typeof decoded.outcome === "object" &&
    decoded.outcome !== null &&
    "kind" in decoded.outcome &&
    typeof decoded.outcome.kind === "string"
  ) {
    return decoded as ExecStreamFrame;
  }
  throw new Error("SecondBox Exec stream server frame is invalid");
}

function decodeTerminalServerFrame(payload: string, expectedSequence: number): TerminalFrame {
  let decoded: unknown;
  try {
    decoded = JSON.parse(payload);
  } catch {
    throw new Error("SecondBox Terminal received invalid JSON");
  }
  if (
    typeof decoded !== "object" ||
    decoded === null ||
    !("type" in decoded) ||
    !("sequence" in decoded) ||
    !Number.isSafeInteger(decoded.sequence) ||
    decoded.sequence !== expectedSequence
  ) {
    throw new Error("SecondBox Terminal server sequence is invalid");
  }
  if (decoded.type === "terminal_output") {
    if (!("dataBase64" in decoded) || typeof decoded.dataBase64 !== "string") {
      throw new Error("SecondBox Terminal output frame is invalid");
    }
    const bytes = decodeBase64(decoded.dataBase64);
    if (encodeBase64(bytes) !== decoded.dataBase64) {
      throw new Error("SecondBox Terminal output is not canonical base64");
    }
    return decoded as TerminalFrame;
  }
  if (
    decoded.type === "outcome" &&
    "outcome" in decoded &&
    typeof decoded.outcome === "object" &&
    decoded.outcome !== null &&
    "kind" in decoded.outcome &&
    typeof decoded.outcome.kind === "string"
  ) {
    return decoded as TerminalFrame;
  }
  throw new Error("SecondBox Terminal server frame is invalid");
}

async function sha256Digest(content: Uint8Array): Promise<string> {
  if (globalThis.crypto?.subtle === undefined) {
    throw new Error("SecondBox Web Crypto SHA-256 support is required for file writes");
  }
  const digest = new Uint8Array(
    await globalThis.crypto.subtle.digest("SHA-256", ownedArrayBuffer(content)),
  );
  let binary = "";
  for (const byte of digest) binary += String.fromCharCode(byte);
  return `sha-256=:${btoa(binary)}:`;
}

function ownedArrayBuffer(content: Uint8Array): ArrayBuffer {
  const copy = new ArrayBuffer(content.byteLength);
  new Uint8Array(copy).set(content);
  return copy;
}

function idempotencyKey(): string {
  if (globalThis.crypto?.randomUUID === undefined) {
    throw new Error("SecondBox Web Crypto randomUUID support is required for mutations");
  }
  return globalThis.crypto.randomUUID();
}

function requirePositiveInteger(value: number, field: string): void {
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`SecondBox ${field} must be a positive integer`);
  }
}

function requireNonempty(value: string, field: string): void {
  if (value === "") {
    throw new Error(`SecondBox ${field} is required`);
  }
}

function requireDurationSeconds(value: number, field: string): void {
  requirePositiveInteger(value, `${field} durationSeconds`);
  if (value > 86_400) {
    throw new Error(`SecondBox ${field} durationSeconds must not exceed 86400`);
  }
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted !== true) return;
  const reason =
    signal.reason instanceof Error && signal.reason.message !== ""
      ? signal.reason.message
      : "The operation was aborted.";
  throw new DOMException(reason, "AbortError");
}

async function abortableDelay(
  milliseconds: number,
  signal: AbortSignal | undefined,
): Promise<void> {
  throwIfAborted(signal);
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(done, milliseconds);
    const onAbort = (): void => {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      try {
        throwIfAborted(signal);
      } catch (error) {
        reject(error);
      }
    };
    function done(): void {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

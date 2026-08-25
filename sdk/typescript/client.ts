import {
  OPERATIONS,
  SecondBoxAPIError,
  SecondBoxClient,
  encodeJSONBody,
  type CreateDirectoryRequest,
  type BufferedExecRequest,
  type Command,
  type CreatePortSessionRequest,
  type CreateProfileRequest,
  type CreateRunnerPoolRequest,
  type CreateSnapshotRequest,
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
  type Profile,
  type ProfilePage,
  type Problem,
  type RelocateSandboxRequest,
  type ReviseProfileRequest,
  type RestoreSnapshotRequest,
  type RemovePathRequest,
  type Sandbox,
  type SandboxPage,
  type SandboxState,
  type Snapshot,
  type SnapshotPage,
  type StreamingExecRequest,
  type TerminalFrame,
  type TerminalAttachedFrame,
  type TerminalSession,
  type TransportRequestOptions,
  type UpdateSandboxMetadataRequest,
  type UpdateRunnerPoolRequest,
  type RunnerPool,
  type RunnerPoolPage,
  type WaitSandboxRequest,
} from "./transport.ts";

export type {
  BufferedExecRequest,
  Command,
  ExecStreamFrame,
  FileStat,
  Lease,
  Metadata,
  Operation,
  PortSession,
  Profile,
  ProfilePage,
  ProfileRevisionSpec,
  Problem,
  RelocateSandboxRequest,
  Sandbox,
  SandboxPage,
  SandboxState,
  Snapshot,
  SnapshotPage,
  RunnerPool,
  RunnerPoolPage,
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

/** Returns one unguessable single-use request key. */
export function newIdempotencyKey(): string {
  return idempotencyKey();
}

/** Renders one Sandbox revision as its If-Match validator. */
export function revisionETag(revision: number): string {
  requirePositiveInteger(revision, "Sandbox revision");
  return `"revision-${revision}"`;
}

/** Returns the typed service problem code carried by the error, or "". */
export function problemCodeOf(error: unknown): string {
  return error instanceof SecondBoxProblemError ? error.problem.code : "";
}

/** Matches the service's public waitForSandbox request bound. */
const MAXIMUM_WAIT_REQUEST_MILLISECONDS = 60_000;

/** Keeps a very short Lease from busy-looping. */
const DEFAULT_MINIMUM_RENEWAL_DELAY_MILLISECONDS = 1_000;

export interface PollOptions {
  readonly intervalMilliseconds: number;
  readonly signal?: AbortSignal;
}

export interface PageOptions {
  readonly limit?: number;
  readonly cursor?: string;
}

export interface SandboxListOptions extends PageOptions {
  readonly metadata?: Metadata;
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
      const problem = await decodeProblemResponse(error.response);
      if (problem === undefined) throw error;
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

  public listProfiles(options: PageOptions = {}, signal?: AbortSignal): Promise<ProfilePage> {
    return this.requestJSON<ProfilePage>("listProfiles", {
      queryParameters: pageQuery(options), signal,
    });
  }

  public getProfile(name: string, signal?: AbortSignal): Promise<Profile> {
    requireNonempty(name, "Profile name");
    return this.requestJSON<Profile>("getProfile", {
      pathParameters: { profileName: name }, signal,
    });
  }

  public async validateProfile(name: string, signal?: AbortSignal): Promise<Profile> {
    const profile = await this.getProfile(name, signal);
    if (profile.state !== "enabled") {
      throw new Error(`SecondBox Profile ${name} is ${profile.state}`);
    }
    return profile;
  }

  public createProfile(
    request: CreateProfileRequest,
    options: { readonly idempotencyKey?: string; readonly signal?: AbortSignal } = {},
  ): Promise<Profile> {
    requireNonempty(request.name, "Profile name");
    return this.requestJSON<Profile>("createProfile", {
      headers: { "Idempotency-Key": options.idempotencyKey ?? idempotencyKey() },
      body: encodeJSONBody(request), signal: options.signal,
    });
  }

  public reviseProfile(
    name: string,
    expectedRevision: number,
    request: ReviseProfileRequest,
    options: { readonly idempotencyKey?: string; readonly signal?: AbortSignal } = {},
  ): Promise<Profile> {
    requireNonempty(name, "Profile name");
    requirePositiveInteger(expectedRevision, "Profile expected revision");
    return this.requestJSON<Profile>("reviseProfile", {
      pathParameters: { profileName: name },
      headers: {
        "If-Match": revisionETag(expectedRevision),
        "Idempotency-Key": options.idempotencyKey ?? idempotencyKey(),
      },
      body: encodeJSONBody(request), signal: options.signal,
    });
  }

  public disableProfile(
    name: string,
    expectedRevision: number,
    options: { readonly idempotencyKey?: string; readonly signal?: AbortSignal } = {},
  ): Promise<Profile> {
    requireNonempty(name, "Profile name");
    requirePositiveInteger(expectedRevision, "Profile expected revision");
    return this.requestJSON<Profile>("disableProfile", {
      pathParameters: { profileName: name },
      headers: {
        "If-Match": revisionETag(expectedRevision),
        "Idempotency-Key": options.idempotencyKey ?? idempotencyKey(),
      },
      signal: options.signal,
    });
  }

  public listRunnerPools(options: PageOptions = {}, signal?: AbortSignal): Promise<RunnerPoolPage> {
    return this.requestJSON<RunnerPoolPage>("listRunnerPools", {
      queryParameters: pageQuery(options), signal,
    });
  }

  public getRunnerPool(name: string, signal?: AbortSignal): Promise<RunnerPool> {
    requireNonempty(name, "RunnerPool name");
    return this.requestJSON<RunnerPool>("getRunnerPool", {
      pathParameters: { runnerPoolName: name }, signal,
    });
  }

  public createRunnerPool(request: CreateRunnerPoolRequest, signal?: AbortSignal): Promise<RunnerPool> {
    requireNonempty(request.name, "RunnerPool name");
    return this.requestJSON<RunnerPool>("createRunnerPool", {
      body: encodeJSONBody(request), signal,
    });
  }

  public updateRunnerPool(
    name: string,
    expectedRevision: number,
    request: UpdateRunnerPoolRequest,
    signal?: AbortSignal,
  ): Promise<RunnerPool> {
    requireNonempty(name, "RunnerPool name");
    requirePositiveInteger(expectedRevision, "RunnerPool expected revision");
    return this.requestJSON<RunnerPool>("updateRunnerPool", {
      pathParameters: { runnerPoolName: name },
      headers: { "If-Match": revisionETag(expectedRevision) },
      body: encodeJSONBody(request), signal,
    });
  }

  public listSandboxes(options: SandboxListOptions = {}): Promise<SandboxPage> {
    const metadata = Object.entries(options.metadata ?? {})
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([name, value]) => `${name}=${value}`);
    if (metadata.length > 8) {
      throw new Error("SecondBox Sandbox metadata filter must not exceed 8 entries");
    }
    return this.requestJSON<SandboxPage>("listSandboxes", {
      queryParameters: { ...pageQuery(options), ...(metadata.length === 0 ? {} : { metadata }) },
      signal: options.signal,
    });
  }

  /** Attaches a handle without taking ownership of the durable Sandbox. */
  public async adoptSandbox(sandboxID: string, signal?: AbortSignal): Promise<SandboxHandle> {
    requireNonempty(sandboxID, "Sandbox ID");
    const sandbox = await this.requestJSON<Sandbox>("getSandbox", {
      pathParameters: { sandboxId: sandboxID }, signal,
    });
    return new SandboxHandle(this, sandbox);
  }

  public getSnapshot(snapshotID: string, signal?: AbortSignal): Promise<Snapshot> {
    requireNonempty(snapshotID, "Snapshot ID");
    return this.requestJSON<Snapshot>("getSnapshot", {
      pathParameters: { snapshotId: snapshotID }, signal,
    });
  }

  public deleteSnapshot(
    snapshotID: string,
    options: { readonly idempotencyKey?: string; readonly signal?: AbortSignal } = {},
  ): Promise<Operation> {
    requireNonempty(snapshotID, "Snapshot ID");
    return this.requestJSON<Operation>("deleteSnapshot", {
      pathParameters: { snapshotId: snapshotID },
      headers: { "Idempotency-Key": options.idempotencyKey ?? idempotencyKey() },
      signal: options.signal,
    });
  }

  public getLease(leaseID: string, signal?: AbortSignal): Promise<Lease> {
    requireNonempty(leaseID, "Lease ID");
    return this.requestJSON<Lease>("getSandboxLease", {
      pathParameters: { leaseId: leaseID },
      signal,
    });
  }

  /**
   * Admits one Sandbox and returns a handle to its representation.
   *
   * The returned Sandbox is the freshly created resource, which is not yet
   * ready; callers wait for the states they require.
   */
  public async createSandbox(
    request: CreateSandboxOptions,
  ): Promise<{ readonly handle: SandboxHandle; readonly operation: Operation }> {
    requireNonempty(request.profile, "Sandbox Profile name");
    const operation = await this.requestJSON<Operation>("createSandbox", {
      headers: { "Idempotency-Key": request.idempotencyKey ?? idempotencyKey() },
      body: encodeJSONBody({
        profile: request.profile,
        metadata: request.metadata ?? {},
        ...(request.sourceSnapshotId === undefined
          ? {}
          : { sourceSnapshotId: request.sourceSnapshotId }),
      }),
      signal: request.signal,
    });
    if (operation.sandboxId === undefined || operation.sandboxId === "") {
      throw new Error("SecondBox Sandbox create returned no Sandbox reference");
    }
    const sandbox = await this.requestJSON<Sandbox>("getSandbox", {
      pathParameters: { sandboxId: operation.sandboxId },
      signal: request.signal,
    });
    return { handle: new SandboxHandle(this, sandbox), operation };
  }

  /**
   * Creates a Sandbox, waits for it to become ready, and executes one command.
   *
   * The Sandbox is deliberately left in place: no handle deletes a Sandbox
   * implicitly. Callers dispose of the returned handle themselves.
   */
  public async run(request: RunRequest): Promise<RunOutcome> {
    requirePositiveInteger(request.deadlineMilliseconds, "run deadlineMilliseconds");
    requirePositiveInteger(request.maximumOutputBytes, "run maximumOutputBytes");
    requirePositiveInteger(request.readyTimeoutMilliseconds, "run readyTimeoutMilliseconds");
    const { handle } = await this.createSandbox({
      profile: request.profile,
      ...(request.metadata === undefined ? {} : { metadata: request.metadata }),
      ...(request.sourceSnapshotId === undefined
        ? {}
        : { sourceSnapshotId: request.sourceSnapshotId }),
      ...(request.signal === undefined ? {} : { signal: request.signal }),
    });
    const sandbox = await handle.waitFor(["ready"], {
      deadlineMilliseconds: request.readyTimeoutMilliseconds,
      ...(request.signal === undefined ? {} : { signal: request.signal }),
    });
    const result = await handle.exec(request.command, {
      ...(request.cwd === undefined ? {} : { cwd: request.cwd }),
      environment: request.environment ?? {},
      ...(request.stdinBase64 === undefined ? {} : { stdinBase64: request.stdinBase64 }),
      deadlineMilliseconds: request.deadlineMilliseconds,
      maximumOutputBytes: request.maximumOutputBytes,
      ...(request.signal === undefined ? {} : { signal: request.signal }),
    });
    return { handle, sandbox, result };
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

export interface ExecOptions extends Omit<BufferedExecRequest, "command"> {
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
      /**
       * Requests replay of output after this last successfully received
       * sequence via the SecondBox-Terminal-After-Sequence header. Absent when
       * the caller attaches without a replay cursor.
       */
      readonly afterSequence?: number;
    },
    signal?: AbortSignal,
  ): Promise<TerminalConnection>;
}

/** Ordered binary-safe helper for one Terminal WebSocket attachment. */
export class Terminal {
  readonly #connection: TerminalConnection;
  #nextClientSequence: number;
  #nextServerSequence: number;
  #terminal = false;
  #writeTail: Promise<void> = Promise.resolve();
  #readTail: Promise<void> = Promise.resolve();

  public constructor(
    connection: TerminalConnection,
    nextClientSequence: number,
    nextServerSequence = 0,
  ) {
    if (connection.subprotocol !== "secondbox.terminal.v1") {
      throw new Error("SecondBox Terminal subprotocol was not negotiated");
    }
    if (!Number.isSafeInteger(nextClientSequence) || nextClientSequence < 0) {
      throw new Error("SecondBox Terminal next client sequence is invalid");
    }
    if (!Number.isSafeInteger(nextServerSequence) || nextServerSequence < 0) {
      throw new Error("SecondBox Terminal next server sequence is invalid");
    }
    this.#connection = connection;
    this.#nextClientSequence = nextClientSequence;
    this.#nextServerSequence = nextServerSequence;
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

/** TLS 1.3 byte stream authenticated with an admitted certificate SPKI pin. */
export interface DirectPortSocket {
  write(payload: Uint8Array): Promise<void>;
  /** Resolves the next available chunk, or an empty array at end of stream. */
  read(signal?: AbortSignal): Promise<Uint8Array>;
  close(): Promise<void>;
}

/**
 * Injects the runtime-specific pinned TLS dialer for the direct Port transport.
 *
 * The dialer completes TLS 1.3 and validates certificateSPKISHA256 before it
 * resolves. This SDK then owns the framed credential handshake, so no
 * credential byte is written before the authenticated transport exists.
 */
export interface DirectPortDialer {
  dial(
    descriptor: {
      readonly host: string;
      readonly port: number;
      readonly certificateSPKISHA256: string;
      readonly sandboxID: string;
      readonly generation: number;
      readonly expiresAt: string;
    },
    signal?: AbortSignal,
  ): Promise<DirectPortSocket>;
}

/** Transports a caller supports. A PortSession is served by exactly one. */
export interface PortTunnelTransports {
  readonly proxied?: PortTunnelConnector;
  readonly direct?: DirectPortDialer;
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
  readFile(path: string, maximumBytes: number, signal?: AbortSignal): Promise<Uint8Array>;
  writeFile(path: string, content: Uint8Array, signal?: AbortSignal): Promise<unknown>;
  statFile(path: string, signal?: AbortSignal): Promise<Pick<FileStat, "kind" | "sizeBytes" | "modifiedAt">>;
  listDirectory(path: string, signal?: AbortSignal): Promise<readonly Pick<FileStat, "path">[]>;
  fileExists(path: string, signal?: AbortSignal): Promise<boolean>;
  createDirectory(path: string, recursive: boolean, signal?: AbortSignal): Promise<void>;
  removePath(path: string, recursive: boolean, force: boolean, signal?: AbortSignal): Promise<void>;
  exec(command: Command, options: ExecOptions): Promise<ExecResult>;
}

export interface LifecycleOptions {
  readonly idempotencyKey?: string;
  readonly expectedRevision?: number;
  /** Raw transport escape hatch; prefer expectedRevision or the observed handle revision. */
  readonly ifMatch?: string;
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

  /** Replaces Metadata against the observed revision without refresh/replay. */
  public async updateMetadata(metadata: Metadata, signal?: AbortSignal): Promise<Sandbox> {
    const sandbox = await this.#api.requestJSON<Sandbox>("updateSandboxMetadata", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: { "If-Match": revisionETag(this.#snapshot.revision) },
      body: encodeJSONBody({ metadata } satisfies UpdateSandboxMetadataRequest),
      signal,
    });
    this.#snapshot = sandbox;
    return sandbox;
  }

  public listSnapshots(options: PageOptions = {}, signal?: AbortSignal): Promise<SnapshotPage> {
    return this.#api.requestJSON<SnapshotPage>("listSandboxSnapshots", {
      pathParameters: { sandboxId: this.#snapshot.id },
      queryParameters: pageQuery(options), signal,
    });
  }

  public createSnapshot(
    request: CreateSnapshotRequest,
    options: LifecycleOptions = {},
  ): Promise<Operation> {
    requireNonempty(request.name, "Snapshot name");
    return this.#api.requestJSON<Operation>("createSandboxSnapshot", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        "If-Match": lifecycleETag(options, this.#snapshot.revision, "createSandboxSnapshot"),
        "Idempotency-Key": options.idempotencyKey ?? idempotencyKey(),
      },
      body: encodeJSONBody(request), signal: options.signal,
    });
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

  /**
   * Blocks until the Sandbox reaches one of the supplied states.
   *
   * The service bounds a single wait request, so this issues repeated bounded
   * waits against the caller's own deadline and reports the last observed state
   * when that deadline passes.
   */
  public async waitFor(
    states: readonly SandboxState[],
    options: WaitForOptions,
  ): Promise<Sandbox> {
    if (states.length === 0) {
      throw new Error("SecondBox Sandbox wait requires at least one state");
    }
    requirePositiveInteger(options.deadlineMilliseconds, "Sandbox waitFor deadlineMilliseconds");
    const target = new Set<SandboxState>(states);
    const expiry = Date.now() + options.deadlineMilliseconds;
    for (;;) {
      if (target.has(this.#snapshot.state)) return this.#snapshot;
      const remaining = expiry - Date.now();
      if (remaining <= 0) {
        throw new Error(
          `SecondBox Sandbox ${this.#snapshot.id} did not reach ${states.join(", ")}: ` +
            `last state=${this.#snapshot.state} generation=${this.#snapshot.generation}`,
        );
      }
      try {
        await this.wait(
          states,
          Math.min(remaining, MAXIMUM_WAIT_REQUEST_MILLISECONDS),
          options.signal,
        );
      } catch (error) {
        if (problemCodeOf(error) !== "wait_expired") throw error;
        await this.refresh(options.signal);
      }
    }
  }

  /**
   * Acquires a Lease and renews it until the keeper is closed.
   *
   * Renewal is driven by the expiry the service actually granted rather than by
   * the requested duration, because the pinned Profile bounds Lease length.
   */
  public async keepLease(
    durationSeconds: number,
    minimumDelayMilliseconds: number = DEFAULT_MINIMUM_RENEWAL_DELAY_MILLISECONDS,
  ): Promise<LeaseKeeper> {
    const lease = await this.acquireLease(durationSeconds, idempotencyKey());
    return new LeaseKeeper(this.#api, lease, durationSeconds, minimumDelayMilliseconds);
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

  public relocate(
    request: RelocateSandboxRequest,
    options: LifecycleOptions,
  ): Promise<Operation> {
    return this.lifecycle("relocateSandbox", options, request as unknown as JSONValue);
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

  public async exec(command: Command, options: ExecOptions): Promise<ExecResult> {
    requirePositiveInteger(options.deadlineMilliseconds, "exec deadlineMilliseconds");
    requirePositiveInteger(options.maximumOutputBytes, "exec maximumOutputBytes");
    const outcome = await this.#api.requestJSON<ExecOutcome>("executeSandboxCommand", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        ...this.dataPlaneHeaders(),
        "Idempotency-Key": idempotencyKey(),
      },
      body: encodeJSONBody({
        command,
        ...(options.cwd === undefined ? {} : { cwd: options.cwd }),
        environment: options.environment,
        ...(options.stdinBase64 === undefined ? {} : { stdinBase64: options.stdinBase64 }),
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

  /** Atomically fences prior Lease authority and acquires its replacement. */
  public takeoverLease(
    durationSeconds: number,
    idempotency: string,
    signal?: AbortSignal,
  ): Promise<Lease> {
    requireDurationSeconds(durationSeconds, "Lease");
    requireNonempty(idempotency, "Lease takeover idempotency key");
    return this.#api.requestJSON<Lease>("acquireSandboxLease", {
      pathParameters: { sandboxId: this.#snapshot.id },
      headers: {
        "SecondBox-Generation": String(this.#snapshot.generation),
        "Idempotency-Key": idempotency,
      },
      body: encodeJSONBody({ durationSeconds, replaceActive: true }),
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
  public connectTerminal(
    session: TerminalSession,
    connector: TerminalConnector,
    signal?: AbortSignal,
  ): Promise<Terminal> {
    return this.attachTerminal(session, undefined, connector, signal);
  }

  /**
   * Attaches and replays output after the last sequence the caller received
   * successfully. Pass -1 when no output has been received.
   */
  public connectTerminalAfter(
    session: TerminalSession,
    afterSequence: number,
    connector: TerminalConnector,
    signal?: AbortSignal,
  ): Promise<Terminal> {
    if (!Number.isSafeInteger(afterSequence) || afterSequence < -1) {
      return Promise.reject(new Error("SecondBox Terminal replay sequence is invalid"));
    }
    return this.attachTerminal(session, afterSequence, connector, signal);
  }

  private async attachTerminal(
    session: TerminalSession,
    afterSequence: number | undefined,
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
        ...(afterSequence === undefined ? {} : { afterSequence }),
      },
      signal,
    );
    const attached = decodeTerminalAttachFrame(await connection.receiveText(signal));
    return new Terminal(
      connection,
      attached.nextClientSequence ?? session.nextClientSequence,
      afterSequence === undefined ? 0 : afterSequence + 1,
    );
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
    connector: PortTunnelConnector | PortTunnelTransports,
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
    const transports = normalizePortTunnelTransports(connector);
    if (session.transport === "direct") {
      return connectDirectPortTunnel(session, endpoint, credential, transports, signal);
    }
    if (!transports.proxied) {
      throw new Error("SecondBox PortSession proxied transport has no connector");
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
    const connection = await transports.proxied.connect(
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

  public async readFile(
    path: string,
    maximumBytes: number,
    signal?: AbortSignal,
  ): Promise<Uint8Array> {
    if (path === "" || !Number.isInteger(maximumBytes) || maximumBytes < 1) {
      throw new Error("SecondBox file path and positive read bound are required");
    }
    const response = await this.#api.request("readSandboxFile", {
      pathParameters: { sandboxId: this.#snapshot.id },
      queryParameters: { path },
      headers: this.dataPlaneHeaders(),
      signal,
    });
    return readBoundedResponse(
      response,
      maximumBytes,
      `SecondBox file read exceeds ${String(maximumBytes)} bytes`,
    );
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
      | "relocateSandbox"
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
        "Idempotency-Key": options.idempotencyKey ?? idempotencyKey(),
        "If-Match": lifecycleETag(options, this.#snapshot.revision, operationID),
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

export interface CreateSandboxOptions {
  readonly profile: string;
  readonly metadata?: Metadata;
  readonly sourceSnapshotId?: string;
  readonly idempotencyKey?: string;
  readonly signal?: AbortSignal;
}

export interface WaitForOptions {
  readonly deadlineMilliseconds: number;
  readonly signal?: AbortSignal;
}

export interface RunRequest extends Omit<BufferedExecRequest, "environment"> {
  readonly profile: string;
  readonly metadata?: Metadata;
  readonly sourceSnapshotId?: string;
  readonly environment?: BufferedExecRequest["environment"];
  readonly readyTimeoutMilliseconds: number;
  readonly signal?: AbortSignal;
}

export interface RunOutcome {
  readonly handle: SandboxHandle;
  readonly sandbox: Sandbox;
  readonly result: ExecResult;
}

/** Holds one Lease active by renewing it before it expires. */
export class LeaseKeeper {
  readonly #api: SecondBox;
  readonly #durationSeconds: number;
  readonly #minimumDelayMilliseconds: number;
  readonly #renewal: Promise<void>;
  #lease: Lease;
  #failure: Error | undefined;
  #closed = false;
  #wake: (() => void) | undefined;

  public constructor(
    api: SecondBox,
    lease: Lease,
    durationSeconds: number,
    minimumDelayMilliseconds: number = DEFAULT_MINIMUM_RENEWAL_DELAY_MILLISECONDS,
  ) {
    this.#api = api;
    this.#lease = lease;
    this.#durationSeconds = durationSeconds;
    this.#minimumDelayMilliseconds = minimumDelayMilliseconds;
    this.#renewal = this.renew();
  }

  public get id(): string {
    return this.#lease.id;
  }

  public get lease(): Lease {
    return this.#lease;
  }

  /** Reports the renewal failure that ended background renewal, if any. */
  public get failure(): Error | undefined {
    return this.#failure;
  }

  private async renew(): Promise<void> {
    while (!this.#closed) {
      await this.sleep(this.delayMilliseconds());
      if (this.#closed) return;
      try {
        this.#lease = await this.#api.renewLease(
          this.#lease.id,
          this.#durationSeconds,
          idempotencyKey(),
        );
      } catch (error) {
        this.#failure = error instanceof Error ? error : new Error(String(error));
        return;
      }
    }
  }

  private sleep(milliseconds: number): Promise<void> {
    return new Promise<void>((resolve) => {
      const timer = setTimeout(() => {
        this.#wake = undefined;
        resolve();
      }, milliseconds);
      this.#wake = () => {
        clearTimeout(timer);
        this.#wake = undefined;
        resolve();
      };
    });
  }

  /** Renews at half the remaining life, with a floor. */
  private delayMilliseconds(): number {
    const remaining = Date.parse(this.#lease.expiresAt) - Date.now();
    if (!Number.isFinite(remaining) || remaining <= 0) {
      return this.#minimumDelayMilliseconds;
    }
    return Math.max(Math.floor(remaining / 2), this.#minimumDelayMilliseconds);
  }

  /**
   * Stops renewal and releases the Lease.
   *
   * A renewal that stopped is why the work failed; releasing a Lease the service
   * has already fenced then fails too, and reporting that consequence instead of
   * its cause sends the caller looking in the wrong place.
   */
  public async close(): Promise<void> {
    if (!this.#closed) {
      this.#closed = true;
      this.#wake?.();
      await this.#renewal;
    }
    let releaseError: unknown;
    try {
      await this.#api.releaseLease(this.#lease.id, idempotencyKey());
    } catch (error) {
      releaseError = error;
    }
    if (this.#failure !== undefined) {
      throw new Error(`SecondBox Lease renewal stopped: ${this.#failure.message}`, {
        cause: this.#failure,
      });
    }
    if (releaseError !== undefined) throw releaseError;
  }
}

export function decodeExecOutcome(outcome: ExecOutcome): ExecResult {
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

/** Bounds Problem decoding the same way the Go transport bounds error bodies. */
const MAXIMUM_PROBLEM_TEXT_LENGTH = 4 << 20;

/**
 * Decodes a non-successful response body as a Problem, or undefined when the
 * body is not a Problem document. A caller receiving undefined must surface
 * the transport error so the HTTP status and body remain observable.
 */
async function decodeProblemResponse(response: Response): Promise<Problem | undefined> {
  const body = await response.clone().text();
  if (body.length > MAXIMUM_PROBLEM_TEXT_LENGTH) return undefined;
  let decoded: unknown;
  try {
    decoded = JSON.parse(body);
  } catch {
    return undefined;
  }
  if (typeof decoded !== "object" || decoded === null || Array.isArray(decoded)) {
    return undefined;
  }
  return decoded as Problem;
}

function decodeTerminalAttachFrame(payload: string): TerminalAttachedFrame {
  let decoded: unknown;
  try {
    decoded = JSON.parse(payload);
  } catch {
    throw new Error("SecondBox Terminal received invalid attach JSON");
  }
  if (
    typeof decoded !== "object" ||
    decoded === null ||
    !("type" in decoded) ||
    decoded.type !== "terminal_attached"
  ) {
    throw new Error("SecondBox Terminal attach response is invalid");
  }
  if (
    "nextClientSequence" in decoded &&
    (!Number.isSafeInteger(decoded.nextClientSequence) || Number(decoded.nextClientSequence) < 0)
  ) {
    throw new Error("SecondBox Terminal attach input sequence is invalid");
  }
  return decoded as TerminalAttachedFrame;
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

function pageQuery(options: PageOptions): Readonly<Record<string, string>> {
  if (options.limit !== undefined &&
      (!Number.isInteger(options.limit) || options.limit < 1 || options.limit > 200)) {
    throw new Error("SecondBox page limit must be an integer from 1 through 200");
  }
  if (options.cursor !== undefined && options.cursor === "") {
    throw new Error("SecondBox page cursor must be nonempty when supplied");
  }
  return {
    ...(options.limit === undefined ? {} : { limit: String(options.limit) }),
    ...(options.cursor === undefined ? {} : { cursor: options.cursor }),
  };
}

function lifecycleETag(
  options: LifecycleOptions,
  observedRevision: number,
  operationID: string,
): string {
  if (options.ifMatch !== undefined && options.expectedRevision !== undefined) {
    throw new Error(`SecondBox ${operationID} cannot combine ifMatch and expectedRevision`);
  }
  if (options.ifMatch !== undefined) return options.ifMatch;
  return revisionETag(options.expectedRevision ?? observedRevision);
}

function ownedArrayBuffer(content: Uint8Array): ArrayBuffer {
  const copy = new ArrayBuffer(content.byteLength);
  new Uint8Array(copy).set(content);
  return copy;
}

async function readBoundedResponse(
  response: Response,
  maximumBytes: number,
  exceededMessage: string,
): Promise<Uint8Array> {
  const reader = response.body?.getReader();
  if (reader === undefined) return new Uint8Array();
  const chunks: Uint8Array[] = [];
  let length = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      length += value.byteLength;
      if (length > maximumBytes) {
        await reader.cancel();
        throw new Error(exceededMessage);
      }
      chunks.push(value);
    }
  } finally {
    reader.releaseLock();
  }
  const content = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    content.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return content;
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

/** Generation one of the direct data-plane handshake. */
const DIRECT_PORT_MAGIC = "SBXDP1";
const DIRECT_PORT_SESSION_KIND_PORT = 0;
const DIRECT_PORT_MAXIMUM_DETAIL_BYTES = 128;
const DIRECT_PORT_VERDICT_ADMITTED = 0;

function normalizePortTunnelTransports(
  connector: PortTunnelConnector | PortTunnelTransports,
): PortTunnelTransports {
  return "connect" in connector ? { proxied: connector } : connector;
}

/**
 * Performs the framed credential handshake, then exposes the socket as an
 * ordinary tunnel connection.
 *
 * The Runner may coalesce its verdict with the first payload bytes, so anything
 * read past the verdict is retained and replayed rather than discarded.
 */
async function connectDirectPortTunnel(
  session: PortSession,
  endpoint: URL,
  credential: string,
  transports: PortTunnelTransports,
  signal?: AbortSignal,
): Promise<PortTunnel> {
  if (!transports.direct) {
    throw new Error("SecondBox PortSession direct transport has no dialer");
  }
  if (
    endpoint.protocol !== "secondbox+tcp:" ||
    endpoint.username !== "" ||
    endpoint.password !== "" ||
    endpoint.search !== "" ||
    endpoint.hostname === ""
  ) {
    throw new Error("SecondBox PortSession direct endpoint is invalid");
  }
  if (credential === "" || credential.length > 2048) {
    throw new Error("SecondBox PortSession endpoint credential is invalid");
  }
  const certificateSPKISHA256 = session.certificateSpkiSha256 ?? "";
  if (!/^[0-9a-f]{64}$/.test(certificateSPKISHA256)) {
    throw new Error("SecondBox PortSession direct certificate SPKI SHA-256 is invalid");
  }
  const port = Number(endpoint.port);
  if (!Number.isSafeInteger(port) || port < 1 || port > 65535) {
    throw new Error("SecondBox PortSession direct endpoint port is invalid");
  }
  const socket = await transports.direct.dial(
    {
      host: endpoint.hostname,
      port,
      certificateSPKISHA256,
      sandboxID: session.sandboxId,
      generation: session.generation,
      expiresAt: session.expiresAt,
    },
    signal,
  );
  try {
    await socket.write(encodeDirectPortCredential(credential));
    const { detail, admitted, remainder } = await readDirectPortVerdict(socket, signal);
    if (!admitted) {
      throw new Error(
        `SecondBox direct Port connection was denied: ${detail || "no detail"}`,
      );
    }
    return new PortTunnel(new DirectPortTunnelConnection(socket, remainder));
  } catch (error) {
    await socket.close().catch(() => undefined);
    throw error;
  }
}

function encodeDirectPortCredential(credential: string): Uint8Array {
  const encoded = new TextEncoder().encode(credential);
  const magic = new TextEncoder().encode(DIRECT_PORT_MAGIC);
  const frame = new Uint8Array(magic.length + 3 + encoded.length);
  frame.set(magic, 0);
  frame[magic.length] = DIRECT_PORT_SESSION_KIND_PORT;
  new DataView(frame.buffer).setUint16(magic.length + 1, encoded.length, false);
  frame.set(encoded, magic.length + 3);
  return frame;
}

async function readDirectPortVerdict(
  socket: DirectPortSocket,
  signal?: AbortSignal,
): Promise<{ admitted: boolean; detail: string; remainder: Uint8Array }> {
  const headerLength = DIRECT_PORT_MAGIC.length + 3;
  let buffered = new Uint8Array(0);
  const need = async (length: number): Promise<void> => {
    while (buffered.length < length) {
      const chunk = await socket.read(signal);
      if (chunk.length === 0) {
        throw new Error("SecondBox direct Port handshake ended before its verdict");
      }
      const grown = new Uint8Array(buffered.length + chunk.length);
      grown.set(buffered, 0);
      grown.set(chunk, buffered.length);
      buffered = grown;
    }
  };
  await need(headerLength);
  const decoder = new TextDecoder();
  if (decoder.decode(buffered.subarray(0, DIRECT_PORT_MAGIC.length)) !== DIRECT_PORT_MAGIC) {
    throw new Error("SecondBox direct Port handshake is malformed");
  }
  const verdict = buffered[DIRECT_PORT_MAGIC.length];
  const detailLength = new DataView(
    buffered.buffer,
    buffered.byteOffset,
    buffered.byteLength,
  ).getUint16(DIRECT_PORT_MAGIC.length + 1, false);
  if (detailLength > DIRECT_PORT_MAXIMUM_DETAIL_BYTES) {
    throw new Error("SecondBox direct Port handshake is malformed");
  }
  await need(headerLength + detailLength);
  return {
    admitted: verdict === DIRECT_PORT_VERDICT_ADMITTED,
    detail: decoder.decode(buffered.subarray(headerLength, headerLength + detailLength)),
    remainder: buffered.slice(headerLength + detailLength),
  };
}

/** Adapts a raw direct socket to the tunnel connection contract. */
class DirectPortTunnelConnection implements PortTunnelConnection {
  readonly subprotocol = "secondbox.port.v1";
  readonly #socket: DirectPortSocket;
  #pending: Uint8Array;

  public constructor(socket: DirectPortSocket, remainder: Uint8Array) {
    this.#socket = socket;
    this.#pending = remainder;
  }

  public sendBinary(payload: Uint8Array): Promise<void> {
    return this.#socket.write(payload);
  }

  public async receiveBinary(signal?: AbortSignal): Promise<Uint8Array> {
    if (this.#pending.length > 0) {
      const pending = this.#pending;
      this.#pending = new Uint8Array(0);
      return pending;
    }
    return this.#socket.read(signal);
  }

  public close(): Promise<void> {
    return this.#socket.close();
  }
}

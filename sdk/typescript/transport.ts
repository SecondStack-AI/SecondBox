export type JSONValue =
  | null
  | boolean
  | number
  | string
  | readonly JSONValue[]
  | { readonly [key: string]: JSONValue };

export type Metadata = Readonly<Record<string, string>>;
export type SandboxState =
  | "creating"
  | "stopped"
  | "starting"
  | "ready"
  | "draining"
  | "stopping"
  | "failed"
  | "deleting"
  | "deleted";
export type ServiceAccountScope =
  | "sandbox:read"
  | "sandbox:lifecycle"
  | "sandbox:exec"
  | "sandbox:files"
  | "sandbox:artifacts"
  | "sandbox:ports";

export interface Problem {
  readonly type: string;
  readonly title: string;
  readonly status: number;
  readonly code: string;
  readonly requestId: string;
  readonly retryable: boolean;
  readonly retryAfterMilliseconds?: number;
}

export interface Sandbox {
  readonly id: string;
  readonly profile: string;
  readonly profileRevisionId: string;
  readonly state: SandboxState;
  readonly desiredState: "running" | "stopped" | "deleted";
  readonly generation: number;
  readonly metadata: Metadata;
  readonly revision: number;
  readonly [key: string]: JSONValue | Metadata | undefined;
}

export interface SandboxPage {
  readonly items: readonly Sandbox[];
  readonly nextCursor?: string;
}

export interface Operation {
  readonly id: string;
  readonly sandboxId: string;
  readonly kind: string;
  readonly state: "pending" | "running" | "succeeded" | "failed" | "cancelled";
  readonly requestId: string;
  readonly sandbox?: Sandbox;
  readonly snapshot?: Snapshot;
  readonly error?: Problem;
  readonly [key: string]: JSONValue | Sandbox | Snapshot | Problem | undefined;
}

export interface Snapshot {
  readonly id: string;
  readonly sandboxId: string;
  readonly generation: number;
  readonly name: string;
  readonly sizeBytes: number;
  readonly metadata: Metadata;
  readonly state: "creating" | "ready" | "deleting" | "failed";
  readonly expiresAt?: string;
  readonly createdAt: string;
}

export interface Project {
  readonly id: string;
  readonly name: string;
}

export interface Profile {
  readonly name: string;
  readonly currentRevision: {
    readonly id: string;
    readonly number: number;
    readonly spec: ProfileRevisionSpec;
  };
}

export interface ServiceAccount {
  readonly id: string;
  readonly projectId: string;
}

export interface CreateAPIKeyResponse {
  readonly apiKey: { readonly id: string };
  readonly credential: string;
}

export interface ProfileRevisionSpec {
  readonly pool: string;
  readonly architecture: "amd64" | "arm64";
  readonly runtimeBundleDigest: string;
  readonly toolchainBundleDigest: string;
  readonly resources: {
    readonly cpuMillis: number;
    readonly memoryBytes: number;
    readonly workspaceBytes: number;
    readonly processLimit: number;
    readonly concurrentOperations: number;
  };
  readonly lifecycle: {
    readonly initialState: "running" | "stopped";
    readonly drainGraceSeconds: number;
    readonly idleSeconds: number;
    readonly maximumDurationSeconds: number;
    readonly leaseSeconds: number;
  };
  readonly retention: {
    readonly snapshotLimit: number;
    readonly snapshotRetentionSeconds: number;
    readonly artifactRetentionSeconds: number;
  };
  readonly execution: {
    readonly maximumDeadlineMilliseconds: number;
    readonly maximumBufferedOutputBytes: number;
    readonly streamWindowBytes: number;
    readonly maximumTransferBytes: number;
    readonly terminalDetachSeconds: number;
  };
  readonly network: {
    readonly mode: "deny_all" | "allow_list";
    readonly destinations: readonly JSONValue[];
  };
  readonly ports: readonly JSONValue[];
}

export interface WaitSandboxRequest {
  readonly states: readonly SandboxState[];
  readonly deadlineMilliseconds: number;
}

export interface RestoreSnapshotRequest {
  readonly snapshotId: string;
}

/** Replaces bounded application correlation metadata under a Sandbox revision fence. */
export interface UpdateSandboxMetadataRequest {
  readonly metadata: Metadata;
}

export interface StreamingExecRequest {
  readonly command: JSONValue;
  readonly cwd?: string;
  readonly environment: Metadata;
  readonly deadlineMilliseconds: number;
  readonly maximumOutputBytes: number;
  readonly windowBytes: number;
}

export interface CreateTerminalRequest {
  readonly command: JSONValue;
  readonly cwd?: string;
  readonly environment: Metadata;
  readonly rows: number;
  readonly columns: number;
  readonly deadlineMilliseconds: number;
  readonly detachable: boolean;
}

export interface CreatePortSessionRequest {
  readonly name: string;
  readonly durationSeconds: number;
}

export interface PortSession {
  readonly id: string;
  readonly sandboxId: string;
  readonly generation: number;
  readonly name: string;
  readonly protocol: "tcp" | "http";
  readonly transport: "relay" | "direct";
  readonly endpoint: string;
  readonly state: "open" | "closing" | "closed" | "expired" | "fenced";
  readonly createdAt: string;
  readonly expiresAt: string;
}

export interface Lease {
  readonly id: string;
  readonly sandboxId: string;
  readonly generation: number;
  readonly state: "active" | "released" | "expired" | "fenced";
  readonly expiresAt: string;
  readonly createdAt: string;
  readonly updatedAt: string;
}

export interface ExecStreamSession {
  readonly id: string;
  readonly sandboxId: string;
  readonly generation: number;
  readonly state: "open" | "closing" | "closed";
  readonly websocketUrl: string;
  readonly subprotocol: "secondbox.exec.v1";
  readonly expiresAt: string;
}

export interface TerminalSession {
  readonly id: string;
  readonly sandboxId: string;
  readonly generation: number;
  readonly state: "open" | "detached" | "closing" | "closed";
  readonly websocketUrl: string;
  readonly subprotocol: "secondbox.terminal.v1";
  readonly streamWindowBytes: number;
  readonly nextClientSequence: number;
  readonly expiresAt: string;
}

export type ExecOutcome =
  | { readonly kind: "exited"; readonly exitCode: number; readonly signal?: number; readonly elapsedMilliseconds: number; readonly output: ExecOutput }
  | { readonly kind: "deadline_exceeded"; readonly elapsedMilliseconds: number; readonly output: ExecOutput }
  | { readonly kind: "cancelled"; readonly output: ExecOutput }
  | { readonly kind: "output_exhausted"; readonly limitBytes: number; readonly output: ExecOutput }
  | { readonly kind: "spawn_failed"; readonly reason: string; readonly message: string }
  | { readonly kind: "infrastructure_failed"; readonly reason: string; readonly message: string; readonly retryable: boolean };

export interface ExecOutput {
  readonly stdoutBase64: string;
  readonly stderrBase64: string;
}

export type ExecStreamFrame =
  | { readonly type: "stdin"; readonly sequence: number; readonly dataBase64: string; readonly endOfInput: boolean }
  | { readonly type: "output"; readonly sequence: number; readonly stream: string; readonly dataBase64: string }
  | { readonly type: "credit"; readonly sequence: number; readonly bytes: number }
  | { readonly type: "signal"; readonly sequence: number; readonly signal: number }
  | { readonly type: "cancel"; readonly sequence: number }
  | { readonly type: "outcome"; readonly sequence: number; readonly outcome: ExecOutcome };

export type TerminalFrame =
  | { readonly type: "terminal_input"; readonly sequence: number; readonly dataBase64: string }
  | { readonly type: "terminal_output"; readonly sequence: number; readonly dataBase64: string }
  | { readonly type: "resize"; readonly sequence: number; readonly rows: number; readonly columns: number }
  | { readonly type: "credit"; readonly sequence: number; readonly bytes: number }
  | { readonly type: "cancel"; readonly sequence: number }
  | { readonly type: "outcome"; readonly sequence: number; readonly outcome: ExecOutcome };

export interface FileStat {
  readonly path: string;
  readonly kind: "file" | "directory" | "symbolic_link";
  readonly sizeBytes: number;
  readonly modifiedAt: string;
}

export interface DirectoryListing {
  readonly path: string;
  readonly entries: readonly FileStat[];
}

export interface FileExistsResult {
  readonly path: string;
  readonly exists: boolean;
}

export interface FileWriteResult {
  readonly path: string;
  readonly sizeBytes: number;
  readonly sha256: string;
}

export interface CreateDirectoryRequest {
  readonly path: string;
  readonly recursive: boolean;
}

export interface RemovePathRequest {
  readonly path: string;
  readonly recursive: boolean;
  readonly force: boolean;
}

export type OperationID =
  | "acquireSandboxLease"
  | "cancelSandboxTerminal"
  | "closeSandboxPortSession"
  | "createProfile"
  | "createSandbox"
  | "createSandboxDirectory"
  | "createSandboxExecStream"
  | "createSandboxPortSession"
  | "createSandboxTerminal"
  | "createSandboxSnapshot"
  | "deleteSnapshot"
  | "deleteSandbox"
  | "drainSandbox"
  | "executeSandboxCommand"
  | "getProfile"
  | "getOperation"
  | "getSandbox"
  | "getSandboxLease"
  | "getSandboxPortSession"
  | "getSnapshot"
  | "listSandboxSnapshots"
  | "listSandboxes"
  | "listSandboxDirectory"
  | "readSandboxFile"
  | "reconnectSandboxTerminal"
  | "releaseSandboxLease"
  | "removeSandboxPath"
  | "renewSandboxLease"
  | "restoreSandboxSnapshot"
  | "sandboxFileExists"
  | "startSandbox"
  | "statSandboxFile"
  | "stopSandbox"
  | "touchSandbox"
  | "updateSandboxMetadata"
  | "waitForSandbox"
  | "writeSandboxFile";

interface Route {
  readonly method: string;
  readonly path: string;
  readonly contentType?: string;
}

export const OPERATIONS: Readonly<Record<OperationID, Route>> = {
  acquireSandboxLease: { method: "POST", path: "/v1/sandboxes/{sandboxId}/leases", contentType: "application/json" },
  cancelSandboxTerminal: { method: "DELETE", path: "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}" },
  closeSandboxPortSession: { method: "DELETE", path: "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}" },
  createProfile: { method: "POST", path: "/v1/profiles", contentType: "application/json" },
  createSandbox: { method: "POST", path: "/v1/sandboxes", contentType: "application/json" },
  createSandboxDirectory: { method: "POST", path: "/v1/sandboxes/{sandboxId}/directories", contentType: "application/json" },
  createSandboxExecStream: { method: "POST", path: "/v1/sandboxes/{sandboxId}/exec-streams", contentType: "application/json" },
  createSandboxPortSession: { method: "POST", path: "/v1/sandboxes/{sandboxId}/port-sessions", contentType: "application/json" },
  createSandboxTerminal: { method: "POST", path: "/v1/sandboxes/{sandboxId}/terminals", contentType: "application/json" },
  createSandboxSnapshot: { method: "POST", path: "/v1/sandboxes/{sandboxId}/snapshots", contentType: "application/json" },
  deleteSnapshot: { method: "DELETE", path: "/v1/snapshots/{snapshotId}" },
  deleteSandbox: { method: "DELETE", path: "/v1/sandboxes/{sandboxId}" },
  drainSandbox: { method: "POST", path: "/v1/sandboxes/{sandboxId}:drain" },
  executeSandboxCommand: { method: "POST", path: "/v1/sandboxes/{sandboxId}/exec", contentType: "application/json" },
  getProfile: { method: "GET", path: "/v1/profiles/{profileName}" },
  getOperation: { method: "GET", path: "/v1/operations/{operationId}" },
  getSandbox: { method: "GET", path: "/v1/sandboxes/{sandboxId}" },
  getSandboxLease: { method: "GET", path: "/v1/leases/{leaseId}" },
  getSandboxPortSession: { method: "GET", path: "/v1/sandboxes/{sandboxId}/port-sessions/{portSessionId}" },
  getSnapshot: { method: "GET", path: "/v1/snapshots/{snapshotId}" },
  listSandboxSnapshots: { method: "GET", path: "/v1/sandboxes/{sandboxId}/snapshots" },
  listSandboxes: { method: "GET", path: "/v1/sandboxes" },
  listSandboxDirectory: { method: "GET", path: "/v1/sandboxes/{sandboxId}/directories" },
  readSandboxFile: { method: "GET", path: "/v1/sandboxes/{sandboxId}/files" },
  reconnectSandboxTerminal: { method: "GET", path: "/v1/sandboxes/{sandboxId}/terminals/{terminalSessionId}" },
  releaseSandboxLease: { method: "DELETE", path: "/v1/leases/{leaseId}" },
  removeSandboxPath: { method: "DELETE", path: "/v1/sandboxes/{sandboxId}/directories", contentType: "application/json" },
  renewSandboxLease: { method: "POST", path: "/v1/leases/{leaseId}:renew", contentType: "application/json" },
  restoreSandboxSnapshot: { method: "POST", path: "/v1/sandboxes/{sandboxId}:restore", contentType: "application/json" },
  sandboxFileExists: { method: "GET", path: "/v1/sandboxes/{sandboxId}/files:exists" },
  startSandbox: { method: "POST", path: "/v1/sandboxes/{sandboxId}:start" },
  statSandboxFile: { method: "GET", path: "/v1/sandboxes/{sandboxId}/files:stat" },
  stopSandbox: { method: "POST", path: "/v1/sandboxes/{sandboxId}:stop" },
  touchSandbox: { method: "POST", path: "/v1/sandboxes/{sandboxId}:touch" },
  updateSandboxMetadata: { method: "PUT", path: "/v1/sandboxes/{sandboxId}/metadata", contentType: "application/json" },
  waitForSandbox: { method: "POST", path: "/v1/sandboxes/{sandboxId}:wait", contentType: "application/json" },
  writeSandboxFile: { method: "PUT", path: "/v1/sandboxes/{sandboxId}/files", contentType: "application/octet-stream" },
};

export interface TransportRequestOptions {
  readonly pathParameters?: Readonly<Record<string, string>>;
  readonly queryParameters?: Readonly<Record<string, string>>;
  readonly headers?: Readonly<Record<string, string>>;
  readonly body?: BodyInit;
  readonly contentType?: string;
  readonly signal?: AbortSignal;
}

export class SecondBoxAPIError extends Error {
  public readonly response: Response;

  public constructor(response: Response) {
    super(`SecondBox API request failed: status=${String(response.status)}`);
    this.name = "SecondBoxAPIError";
    this.response = response;
  }
}

export class SecondBoxClient {
  readonly #baseURL: URL;
  readonly #token: string;
  readonly #fetch: typeof fetch;
  readonly #tenantRef: string;
  readonly #subjectRef: string;

  public constructor(
    rawURL: string,
    token: string,
    fetcher: typeof fetch,
    tenantRef = "secondbox",
    subjectRef = "secondbox-admin",
  ) {
    const baseURL = new URL(rawURL);
    if (!["http:", "https:"].includes(baseURL.protocol) || baseURL.search !== "" || baseURL.hash !== "") {
      throw new Error("SecondBox client URL must be an absolute HTTP endpoint without query or fragment");
    }
    if (token === "" || tenantRef === "" || subjectRef === "") {
      throw new Error("SecondBox client token, tenant reference, and subject reference are required");
    }
    this.#baseURL = baseURL;
    this.#token = token;
    this.#fetch = fetcher;
    this.#tenantRef = tenantRef;
    this.#subjectRef = subjectRef;
  }

  public async send(route: Route, options: TransportRequestOptions = {}): Promise<Response> {
    let path = route.path;
    for (const match of path.matchAll(/\{([^}]+)\}/g)) {
      const name = match[1];
      if (name === undefined) throw new Error("SecondBox client path template is malformed");
      const value = options.pathParameters?.[name];
      if (value === undefined || value === "") {
        throw new Error(`SecondBox client missing required path parameter ${name}`);
      }
      path = path.replace(`{${name}}`, encodeURIComponent(value));
    }
    const endpoint = new URL(path, this.#baseURL);
    for (const [name, value] of Object.entries(options.queryParameters ?? {})) {
      endpoint.searchParams.append(name, value);
    }
    const headers = new Headers(options.headers);
    headers.set("Authorization", `Bearer ${this.#token}`);
    headers.set("X-SecondBox-Tenant-Ref", this.#tenantRef);
    headers.set("X-SecondBox-Subject-Ref", this.#subjectRef);
    const contentType = options.contentType ?? (options.body === undefined ? undefined : route.contentType);
    if (contentType !== undefined) headers.set("Content-Type", contentType);
    const response = await this.#fetch(endpoint, {
      method: route.method,
      headers,
      ...(options.body === undefined ? {} : { body: options.body }),
      ...(options.signal === undefined ? {} : { signal: options.signal }),
    });
    if (!response.ok) throw new SecondBoxAPIError(response);
    return response;
  }
}

export function encodeJSONBody(value: unknown): string {
  return JSON.stringify(value);
}

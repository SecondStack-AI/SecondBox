import { createHash, timingSafeEqual, X509Certificate } from "node:crypto";
import type { IncomingMessage } from "node:http";
import { connect as connectTLS, type TLSSocket } from "node:tls";
import WebSocket, { type RawData } from "ws";

import {
  SecondBoxProblemError,
  type DirectPortDialer,
  type DirectPortSocket,
  type ExecStreamConnection,
  type ExecStreamConnector,
  type PortTunnelConnection,
  type PortTunnelConnector,
  type PortTunnelTransports,
  type Problem,
  type TerminalConnection,
  type TerminalConnector,
} from "./client.ts";
import { SecondBoxAPIError } from "./transport.ts";

const websocketMessageQueues = new WeakMap<
  WebSocket,
  AsyncQueue<{ data: RawData; isBinary: boolean }>
>();

export interface NodeTransportAuthority {
  readonly token: string;
  readonly tenantRef: string;
  readonly subjectRef: string;
}

export interface NodeSecondBoxTransports extends PortTunnelTransports {
  readonly exec: ExecStreamConnector;
  readonly terminal: TerminalConnector;
  readonly proxied: PortTunnelConnector;
  readonly direct: DirectPortDialer;
}

/**
 * Creates Node-specific authenticated WebSocket and pinned direct-port
 * transports. Browser-neutral transport contracts remain in the main export.
 */
export function createNodeTransports(
  authority: NodeTransportAuthority,
): NodeSecondBoxTransports {
  requireAuthority(authority);
  const authenticatedHeaders = (generation: number): Readonly<Record<string, string>> => ({
    Authorization: `Bearer ${authority.token}`,
    "X-SecondBox-Tenant-Ref": authority.tenantRef,
    "X-SecondBox-Subject-Ref": authority.subjectRef,
    "SecondBox-Generation": String(generation),
  });
  const exec: ExecStreamConnector = {
    async connect(descriptor, signal) {
      const socket = await openWebSocket(
        descriptor.websocketURL,
        [descriptor.subprotocol],
        authenticatedHeaders(descriptor.generation),
        signal,
      );
      return new NodeTextConnection(socket);
    },
  };
  const terminal: TerminalConnector = {
    async connect(descriptor, signal) {
      const socket = await openWebSocket(
        descriptor.websocketURL,
        [descriptor.subprotocol],
        {
          ...authenticatedHeaders(descriptor.generation),
          ...(descriptor.afterSequence === undefined
            ? {}
            : { "SecondBox-Terminal-After-Sequence": String(descriptor.afterSequence) }),
        },
        signal,
      );
      return new NodeTextConnection(socket);
    },
  };
  const proxied: PortTunnelConnector = {
    async connect(descriptor, signal) {
      const socket = await openWebSocket(
        descriptor.websocketURL,
        descriptor.subprotocols,
        {},
        signal,
      );
      return new NodeBinaryConnection(socket);
    },
  };
  const direct: DirectPortDialer = {
    dial: openPinnedTLSSocket,
  };
  return { exec, terminal, proxied, direct };
}

class NodeTextConnection implements ExecStreamConnection, TerminalConnection {
  readonly #socket: WebSocket;
  readonly #messages: AsyncQueue<{ data: RawData; isBinary: boolean }>;

  public constructor(socket: WebSocket) {
    this.#socket = socket;
    this.#messages = websocketMessages(socket);
  }

  public get subprotocol(): string {
    return this.#socket.protocol;
  }

  public sendText(payload: string): Promise<void> {
    return sendWebSocket(this.#socket, payload);
  }

  public async receiveText(signal?: AbortSignal): Promise<string> {
    const message = await this.#messages.shift(signal);
    if (message.isBinary) {
      throw new Error("SecondBox Node WebSocket received binary data on a text transport");
    }
    return rawDataBytes(message.data).toString("utf8");
  }

  public close(): Promise<void> {
    return closeWebSocket(this.#socket);
  }
}

class NodeBinaryConnection implements PortTunnelConnection {
  readonly #socket: WebSocket;
  readonly #messages: AsyncQueue<{ data: RawData; isBinary: boolean }>;

  public constructor(socket: WebSocket) {
    this.#socket = socket;
    this.#messages = websocketMessages(socket);
  }

  public get subprotocol(): string {
    return this.#socket.protocol;
  }

  public sendBinary(payload: Uint8Array): Promise<void> {
    return sendWebSocket(this.#socket, Buffer.from(payload));
  }

  public async receiveBinary(signal?: AbortSignal): Promise<Uint8Array> {
    const message = await this.#messages.shift(signal);
    if (!message.isBinary) {
      throw new Error("SecondBox Node WebSocket received text data on a binary transport");
    }
    return ownedBytes(rawDataBytes(message.data));
  }

  public close(): Promise<void> {
    return closeWebSocket(this.#socket);
  }
}

class NodeDirectPortSocket implements DirectPortSocket {
  readonly #socket: TLSSocket;
  readonly #chunks = new AsyncQueue<Buffer>();

  public constructor(socket: TLSSocket) {
    this.#socket = socket;
    socket.on("data", (chunk: Buffer) => this.#chunks.push(chunk));
    socket.on("end", () => this.#chunks.end());
    socket.on("error", (error) => this.#chunks.fail(error));
  }

  public write(payload: Uint8Array): Promise<void> {
    if (this.#socket.destroyed) {
      return Promise.reject(new Error("SecondBox Node direct Port socket is closed"));
    }
    return new Promise((resolve, reject) => {
      this.#socket.write(Buffer.from(payload), (error?: Error | null) => {
        if (error) reject(error);
        else resolve();
      });
    });
  }

  public async read(signal?: AbortSignal): Promise<Uint8Array> {
    const chunk = await this.#chunks.shift(signal);
    return ownedBytes(chunk);
  }

  public close(): Promise<void> {
    if (this.#socket.destroyed) return Promise.resolve();
    return new Promise((resolve) => {
      this.#socket.once("close", resolve);
      this.#socket.end();
    });
  }
}

async function openPinnedTLSSocket(
  descriptor: Parameters<DirectPortDialer["dial"]>[0],
  signal?: AbortSignal,
): Promise<DirectPortSocket> {
  throwIfAborted(signal);
  return new Promise((resolve, reject) => {
    const socket = connectTLS({
      host: descriptor.host,
      port: descriptor.port,
      minVersion: "TLSv1.3",
      rejectUnauthorized: false,
    });
    let settled = false;
    const finish = (error?: Error): void => {
      if (settled) return;
      settled = true;
      signal?.removeEventListener("abort", abort);
      socket.removeListener("error", fail);
      if (error) {
        socket.destroy();
        reject(error);
      } else {
        resolve(new NodeDirectPortSocket(socket));
      }
    };
    const fail = (error: Error): void => finish(error);
    const abort = (): void => finish(abortError(signal));
    socket.once("error", fail);
    signal?.addEventListener("abort", abort, { once: true });
    socket.once("secureConnect", () => {
      try {
        const peer = socket.getPeerCertificate(true);
        if (peer.raw === undefined) {
          throw new Error("SecondBox Node direct Port TLS peer presented no certificate");
        }
        const spki = new X509Certificate(peer.raw).publicKey.export({
          format: "der",
          type: "spki",
        });
        const actual = createHash("sha256").update(spki).digest();
        const expected = Buffer.from(descriptor.certificateSPKISHA256, "hex");
        if (expected.length !== actual.length || !timingSafeEqual(expected, actual)) {
          throw new Error("SecondBox Node direct Port TLS certificate SPKI does not match the admitted pin");
        }
        finish();
      } catch (error) {
        finish(error instanceof Error ? error : new Error(String(error)));
      }
    });
  });
}

function openWebSocket(
  url: string,
  protocols: readonly string[],
  headers: Readonly<Record<string, string>>,
  signal?: AbortSignal,
): Promise<WebSocket> {
  throwIfAborted(signal);
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(url, [...protocols], { headers });
    // Install buffering before the open event so a peer's immediate first
    // frame cannot race construction of the public connection wrapper.
    websocketMessages(socket);
    let settled = false;
    const finish = (error?: Error): void => {
      if (settled) return;
      settled = true;
      signal?.removeEventListener("abort", abort);
      socket.removeListener("open", opened);
      socket.removeListener("error", failed);
      socket.removeListener("unexpected-response", rejected);
      if (error) {
        socket.terminate();
        reject(error);
      } else {
        resolve(socket);
      }
    };
    const opened = (): void => {
      if (!protocols.includes(socket.protocol)) {
        finish(new Error("SecondBox Node WebSocket did not negotiate a requested subprotocol"));
        return;
      }
      finish();
    };
    const failed = (error: Error): void => finish(error);
    const rejected = (_request: unknown, response: IncomingMessage): void => {
      void (async () => {
        try {
          const body = await readBoundedHandshakeBody(response);
          finish(handshakeFailure(response.statusCode ?? 0, body));
        } catch (error) {
          finish(error instanceof Error ? error : new Error(String(error)));
        }
      })();
    };
    const abort = (): void => finish(abortError(signal));
    socket.once("open", opened);
    socket.once("error", failed);
    socket.once("unexpected-response", rejected);
    signal?.addEventListener("abort", abort, { once: true });
  });
}

/** Matches the Go SDK's bound on WebSocket attach error responses. */
const MAXIMUM_HANDSHAKE_PROBLEM_BYTES = 4 << 20;

function readBoundedHandshakeBody(response: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    response.on("data", (chunk: Buffer) => {
      total += chunk.byteLength;
      if (total > MAXIMUM_HANDSHAKE_PROBLEM_BYTES) {
        response.destroy();
        reject(new Error(
          `SecondBox Node WebSocket handshake error response exceeds ${String(MAXIMUM_HANDSHAKE_PROBLEM_BYTES)} bytes`,
        ));
        return;
      }
      chunks.push(chunk);
    });
    response.on("end", () => resolve(Buffer.concat(chunks)));
    response.on("error", reject);
  });
}

/**
 * Surfaces a rejected WebSocket handshake the same way the Go SDK does: a
 * typed Problem error when the body decodes as one, otherwise a structured
 * API error that preserves the HTTP status and bounded body.
 */
function handshakeFailure(status: number, body: Buffer): Error {
  const text = body.toString("utf8");
  let decoded: unknown;
  try {
    decoded = JSON.parse(text);
  } catch {
    decoded = undefined;
  }
  if (typeof decoded === "object" && decoded !== null && !Array.isArray(decoded)) {
    return new SecondBoxProblemError(status, decoded as Problem);
  }
  return new SecondBoxAPIError(new Response(text === "" ? null : text, { status }));
}

function websocketMessages(
  socket: WebSocket,
): AsyncQueue<{ data: RawData; isBinary: boolean }> {
  const existing = websocketMessageQueues.get(socket);
  if (existing) return existing;
  const messages = new AsyncQueue<{ data: RawData; isBinary: boolean }>();
  websocketMessageQueues.set(socket, messages);
  socket.on("message", (data, isBinary) => messages.push({ data, isBinary }));
  socket.on("close", () => messages.end());
  socket.on("error", (error) => messages.fail(error));
  return messages;
}

function sendWebSocket(socket: WebSocket, payload: string | Buffer): Promise<void> {
  if (socket.readyState !== WebSocket.OPEN) {
    return Promise.reject(new Error("SecondBox Node WebSocket is not open"));
  }
  return new Promise((resolve, reject) => {
    socket.send(payload, (error?: Error) => {
      if (error) reject(error);
      else resolve();
    });
  });
}

function closeWebSocket(socket: WebSocket): Promise<void> {
  if (socket.readyState === WebSocket.CLOSED) return Promise.resolve();
  return new Promise((resolve) => {
    socket.once("close", resolve);
    socket.close();
  });
}

class AsyncQueue<T> {
  readonly #values: T[] = [];
  readonly #waiters: Array<{
    resolve: (value: T) => void;
    reject: (error: Error) => void;
  }> = [];
  #terminal?: Error;

  public push(value: T): void {
    const waiter = this.#waiters.shift();
    if (waiter) waiter.resolve(value);
    else this.#values.push(value);
  }

  public end(): void {
    this.fail(new Error("SecondBox Node transport reached end of stream"));
  }

  public fail(error: Error): void {
    if (this.#terminal) return;
    this.#terminal = error;
    for (const waiter of this.#waiters.splice(0)) waiter.reject(error);
  }

  public shift(signal?: AbortSignal): Promise<T> {
    throwIfAborted(signal);
    const value = this.#values.shift();
    if (value !== undefined) return Promise.resolve(value);
    if (this.#terminal) return Promise.reject(this.#terminal);
    return new Promise((resolve, reject) => {
      const waiter = {
        resolve: (item: T) => {
          signal?.removeEventListener("abort", abort);
          resolve(item);
        },
        reject: (error: Error) => {
          signal?.removeEventListener("abort", abort);
          reject(error);
        },
      };
      const abort = (): void => {
        const index = this.#waiters.indexOf(waiter);
        if (index >= 0) this.#waiters.splice(index, 1);
        reject(abortError(signal));
      };
      this.#waiters.push(waiter);
      signal?.addEventListener("abort", abort, { once: true });
    });
  }
}

function rawDataBytes(data: RawData): Buffer {
  if (Array.isArray(data)) return Buffer.concat(data);
  return Buffer.isBuffer(data) ? data : Buffer.from(data);
}

function ownedBytes(value: Uint8Array): Uint8Array {
  const copy = new Uint8Array(value.byteLength);
  copy.set(value);
  return copy;
}

function requireAuthority(authority: NodeTransportAuthority): void {
  if (authority.token === "" || authority.tenantRef === "" || authority.subjectRef === "") {
    throw new Error("SecondBox Node transport token, tenant reference, and subject reference are required");
  }
}

function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw abortError(signal);
}

function abortError(signal?: AbortSignal): DOMException {
  const error = new DOMException("The SecondBox Node transport operation was aborted", "AbortError");
  Object.defineProperty(error, "cause", { value: signal?.reason, configurable: true });
  return error;
}

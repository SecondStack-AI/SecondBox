import {
  createSandboxSessionEnv,
  type FileStat as FlueFileStat,
  type SandboxApi,
  type SandboxFactory,
} from "@flue/runtime";

import type {
  ExecResult,
  SandboxFilesystem,
} from "./client.ts";

const FLUE_WORKSPACE_ROOT = "/workspace";

export interface SecondBoxFlueOptions {
  readonly defaultDeadlineMilliseconds: number;
  readonly maximumOutputBytes: number;
}

/** Exact Flue SandboxApi mapping over an already initialized SecondBox Sandbox handle. */
export class SecondBoxFlueSandboxApi implements SandboxApi {
  readonly #sandbox: SandboxFilesystem;
  readonly #options: SecondBoxFlueOptions;

  public constructor(
    sandbox: SandboxFilesystem,
    options: SecondBoxFlueOptions,
  ) {
    requirePositiveInteger(
      options.defaultDeadlineMilliseconds,
      "Flue defaultDeadlineMilliseconds",
    );
    requirePositiveInteger(options.maximumOutputBytes, "Flue maximumOutputBytes");
    this.#sandbox = sandbox;
    this.#options = options;
  }

  public async readFile(path: string): Promise<string> {
    return new TextDecoder().decode(await this.readFileBuffer(path));
  }

  public readFileBuffer(path: string): Promise<Uint8Array> {
    return this.#sandbox.readFile(workspaceRelativePath(path));
  }

  public async writeFile(
    path: string,
    content: string | Uint8Array,
  ): Promise<void> {
    const bytes =
      typeof content === "string" ? new TextEncoder().encode(content) : content;
    await this.#sandbox.writeFile(workspaceRelativePath(path), bytes);
  }

  public async stat(path: string): Promise<FlueFileStat> {
    const stat = await this.#sandbox.statFile(workspaceRelativePath(path));
    const mtime = new Date(stat.modifiedAt);
    if (Number.isNaN(mtime.getTime())) {
      throw new Error(`SecondBox Flue adapter received invalid modifiedAt ${stat.modifiedAt}`);
    }
    return {
      isFile: stat.kind === "file",
      isDirectory: stat.kind === "directory",
      isSymbolicLink: stat.kind === "symbolic_link",
      size: stat.sizeBytes,
      mtime,
    };
  }

  public async readdir(path: string): Promise<string[]> {
    const entries = await this.#sandbox.listDirectory(workspaceRelativePath(path));
    return entries.map((entry) => basename(entry.path));
  }

  public exists(path: string): Promise<boolean> {
    return this.#sandbox.fileExists(workspaceRelativePath(path));
  }

  public mkdir(
    path: string,
    options?: { recursive?: boolean },
  ): Promise<void> {
    return this.#sandbox.createDirectory(
      workspaceRelativePath(path),
      options?.recursive === true,
    );
  }

  public rm(
    path: string,
    options?: { recursive?: boolean; force?: boolean },
  ): Promise<void> {
    return this.#sandbox.removePath(
      workspaceRelativePath(path),
      options?.recursive === true,
      options?.force === true,
    );
  }

  public async exec(
    command: string,
    options?: {
      cwd?: string;
      env?: Record<string, string>;
      timeoutMs?: number;
      signal?: AbortSignal;
    },
  ): Promise<{ stdout: string; stderr: string; exitCode: number }> {
    const timeout =
      options?.timeoutMs ?? this.#options.defaultDeadlineMilliseconds;
    requirePositiveInteger(timeout, "Flue exec timeoutMs");
    const outcome = await this.#sandbox.exec(command, {
      ...(options?.cwd === undefined
        ? {}
        : { cwd: workspaceRelativePath(options.cwd) }),
      environment: options?.env ?? {},
      deadlineMilliseconds: timeout,
      maximumOutputBytes: this.#options.maximumOutputBytes,
      signal: options?.signal,
    });
    return flueShellResult(outcome);
  }
}

/** Creates a Flue factory without acquiring or owning provider infrastructure. */
export function createSecondBoxFlueAdapter(
  sandbox: SandboxFilesystem,
  options: SecondBoxFlueOptions,
): SandboxFactory {
  return {
    async createSessionEnv() {
      return createSandboxSessionEnv(
        new SecondBoxFlueSandboxApi(sandbox, options),
        FLUE_WORKSPACE_ROOT,
      );
    },
  };
}

function workspaceRelativePath(path: string): string {
  if (path === FLUE_WORKSPACE_ROOT) return ".";
  const prefix = `${FLUE_WORKSPACE_ROOT}/`;
  if (!path.startsWith(prefix)) {
    throw new Error(
      `SecondBox Flue path ${JSON.stringify(path)} is outside the SecondBox workspace root ${FLUE_WORKSPACE_ROOT}`,
    );
  }
  const relative = path.slice(prefix.length);
  if (
    relative === "" ||
    relative.split("/").some((component) => component === "..")
  ) {
    throw new Error(`SecondBox Flue path ${JSON.stringify(path)} is invalid`);
  }
  return relative;
}

function basename(path: string): string {
  const components = path.split("/");
  const name = components.at(-1);
  if (name === undefined || name === "") {
    throw new Error(`SecondBox Flue adapter received invalid directory entry ${path}`);
  }
  return name;
}

function flueShellResult(
  outcome: ExecResult,
): { stdout: string; stderr: string; exitCode: number } {
  switch (outcome.kind) {
    case "exited":
      return {
        stdout: text(outcome.stdout),
        stderr: text(outcome.stderr),
        exitCode: outcome.exitCode,
      };
    case "deadline_exceeded":
      return {
        stdout: text(outcome.stdout),
        stderr: appendDiagnostic(
          text(outcome.stderr),
          `SecondBox command deadline exceeded after ${outcome.elapsedMilliseconds} ms`,
        ),
        exitCode: 124,
      };
    case "cancelled":
      return {
        stdout: text(outcome.stdout),
        stderr: appendDiagnostic(text(outcome.stderr), "SecondBox command cancelled"),
        exitCode: 130,
      };
    case "output_exhausted":
      return {
        stdout: text(outcome.stdout),
        stderr: appendDiagnostic(
          text(outcome.stderr),
          `SecondBox command output exceeded ${outcome.limitBytes} bytes`,
        ),
        exitCode: 1,
      };
    case "spawn_failed":
      return {
        stdout: "",
        stderr: outcome.message,
        exitCode: outcome.reason === "not_found" ? 127 : 126,
      };
    case "infrastructure_failed":
      throw new Error(
        `SecondBox command infrastructure failure: reason=${outcome.reason} retryable=${outcome.retryable} message=${outcome.message}`,
      );
  }
}

function text(value: Uint8Array): string {
  return new TextDecoder().decode(value);
}

function appendDiagnostic(stderr: string, diagnostic: string): string {
  if (stderr === "") return diagnostic;
  if (stderr.endsWith("\n")) return `${stderr}${diagnostic}`;
  return `${stderr}\n${diagnostic}`;
}

function requirePositiveInteger(value: number, field: string): void {
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`SecondBox ${field} must be a positive integer`);
  }
}

/*
 * SPDX-License-Identifier: Apache-2.0
 *
 * Adapted from @flue/runtime 1.0.0-beta.9:
 * https://github.com/withastro/flue/tree/v1.0.0-beta.9/packages/runtime/src
 *
 * SecondBox changes: retain only the structural sandbox contracts and the
 * createSandboxSessionEnv behavior required by the adapter. Formatting and
 * exported names follow this repository. See flue-runtime-beta9-source.json.
 */

export interface FileStat {
  isFile: boolean;
  isDirectory: boolean;
  isSymbolicLink?: boolean;
  size?: number;
  mtime?: Date;
}

export interface ShellResult {
  stdout: string;
  stderr: string;
  exitCode: number;
}

export interface SessionEnv {
  exec(
    command: string,
    options?: {
      cwd?: string;
      env?: Record<string, string>;
      timeoutMs?: number;
      signal?: AbortSignal;
    },
  ): Promise<ShellResult>;
  readFile(path: string): Promise<string>;
  readFileBuffer(path: string): Promise<Uint8Array>;
  writeFile(path: string, content: string | Uint8Array): Promise<void>;
  stat(path: string): Promise<FileStat>;
  readdir(path: string): Promise<string[]>;
  exists(path: string): Promise<boolean>;
  mkdir(path: string, options?: { recursive?: boolean }): Promise<void>;
  rm(
    path: string,
    options?: { recursive?: boolean; force?: boolean },
  ): Promise<void>;
  cwd: string;
  resolvePath(path: string): string;
}

export interface SandboxApi {
  readFile(path: string): Promise<string>;
  readFileBuffer(path: string): Promise<Uint8Array>;
  writeFile(path: string, content: string | Uint8Array): Promise<void>;
  stat(path: string): Promise<FileStat>;
  readdir(path: string): Promise<string[]>;
  exists(path: string): Promise<boolean>;
  mkdir(path: string, options?: { recursive?: boolean }): Promise<void>;
  rm(
    path: string,
    options?: { recursive?: boolean; force?: boolean },
  ): Promise<void>;
  exec(
    command: string,
    options?: {
      cwd?: string;
      env?: Record<string, string>;
      timeoutMs?: number;
      signal?: AbortSignal;
    },
  ): Promise<ShellResult>;
}

export interface SandboxFactory<TSessionToolFactory = never> {
  createSessionEnv(options: { id: string }): Promise<SessionEnv>;
  tools?: TSessionToolFactory;
}

/** Wraps the frozen Flue beta.9 SandboxApi shape into its SessionEnv shape. */
export function createSandboxSessionEnv(
  api: SandboxApi,
  cwd: string,
): SessionEnv {
  const resolvePath = makeResolvePath(cwd);
  return {
    async exec(command, options) {
      const signal = options?.signal;
      if (signal?.aborted) throw abortErrorFor(signal);
      const result = await api.exec(command, {
        cwd: options?.cwd !== undefined ? resolvePath(options.cwd) : cwd,
        env: options?.env,
        timeoutMs: options?.timeoutMs,
        signal,
      });
      if (signal?.aborted) throw abortErrorFor(signal);
      return result;
    },
    async readFile(path) {
      return api.readFile(resolvePath(path));
    },
    async readFileBuffer(path) {
      return api.readFileBuffer(resolvePath(path));
    },
    async writeFile(path, content) {
      const resolved = resolvePath(path);
      return writeFileCreatingParents(
        () => api.writeFile(resolved, content),
        () => api.mkdir(posixParentDir(resolved), { recursive: true }),
      );
    },
    async stat(path) {
      return api.stat(resolvePath(path));
    },
    async readdir(path) {
      return api.readdir(resolvePath(path));
    },
    async exists(path) {
      return api.exists(resolvePath(path));
    },
    async mkdir(path, options) {
      return api.mkdir(resolvePath(path), options);
    },
    async rm(path, options) {
      return api.rm(resolvePath(path), options);
    },
    cwd,
    resolvePath,
  };
}

async function writeFileCreatingParents(
  write: () => Promise<void>,
  mkdirParent: () => Promise<unknown>,
): Promise<void> {
  try {
    await write();
    return;
  } catch {
    // Flue beta.9 retries after attempting to create the missing parent.
  }
  try {
    await mkdirParent();
  } catch {
    // The retried write remains the authoritative error.
  }
  await write();
}

function posixParentDir(path: string): string {
  return path.replace(/\/[^/]*$/, "") || "/";
}

function normalizePath(path: string): string {
  const normalizedComponents: string[] = [];
  for (const component of path.split("/")) {
    if (component === "." || component === "") continue;
    if (component === "..") {
      normalizedComponents.pop();
    } else {
      normalizedComponents.push(component);
    }
  }
  return `/${normalizedComponents.join("/")}`;
}

function makeResolvePath(cwd: string): (path: string) => string {
  return (path: string): string => {
    if (path.startsWith("/")) return normalizePath(path);
    if (cwd === "/") return normalizePath(`/${path}`);
    return normalizePath(`${cwd}/${path}`);
  };
}

function abortErrorFor(signal: AbortSignal): Error {
  const reason = signal.reason;
  const message =
    reason instanceof Error && reason.message
      ? reason.message
      : typeof reason === "string" && reason
        ? reason
        : "The operation was aborted.";
  const error = new DOMException(message, "AbortError");
  try {
    Object.defineProperty(error, "cause", {
      value: reason,
      configurable: true,
    });
  } catch {
    // Flue beta.9 leaves cause unset when DOMException makes it read-only.
  }
  return error;
}

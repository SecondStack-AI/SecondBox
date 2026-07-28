import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import test from "node:test";

import {
  createSandboxSessionEnv,
  type FileStat,
  type SandboxApi,
  type SandboxFactory,
  type SessionEnv,
  type ShellResult,
} from "./flue-runtime-beta9-compat.ts";

test("frozen Flue beta.9 session resolves paths and exposes the exact session shape", async () => {
  const sandbox = new RecordingFlueBeta9Sandbox();
  const session = createSandboxSessionEnv(sandbox, "/workspace");

  assert.equal(session.cwd, "/workspace");
  assert.equal(session.resolvePath("nested/../value.txt"), "/workspace/value.txt");
  assert.equal(session.resolvePath("/absolute/./value.txt"), "/absolute/value.txt");
  await session.readFile("nested/../value.txt");
  await session.mkdir("directory", { recursive: true });
  await session.rm("directory", { recursive: true, force: true });

  assert.deepEqual(sandbox.calls, [
    ["readFile", "/workspace/value.txt"],
    ["mkdir", "/workspace/directory", { recursive: true }],
    ["rm", "/workspace/directory", { recursive: true, force: true }],
  ]);

  const structuralFactory: SandboxFactory = {
    async createSessionEnv(_options: { id: string }): Promise<SessionEnv> {
      return session;
    },
  };
  const beta9Consumer: {
    createSessionEnv(options: { id: string }): Promise<SessionEnv>;
    tools?: (env: SessionEnv, options: { subagents: Record<string, unknown> }) => unknown[];
  } = structuralFactory;
  assert.equal((await beta9Consumer.createSessionEnv({ id: "workflow" })).cwd, "/workspace");
});

test("frozen Flue beta.9 write creates a missing parent and retries exactly once", async () => {
  const sandbox = new RecordingFlueBeta9Sandbox();
  sandbox.failFirstWrite = true;
  const session = createSandboxSessionEnv(sandbox, "/workspace");

  await session.writeFile("nested/value.txt", "value");

  assert.deepEqual(sandbox.calls, [
    ["writeFile", "/workspace/nested/value.txt", "value"],
    ["mkdir", "/workspace/nested", { recursive: true }],
    ["writeFile", "/workspace/nested/value.txt", "value"],
  ]);
});

test("frozen Flue beta.9 session rejects pre-aborted and post-aborted exec calls", async () => {
  const sandbox = new RecordingFlueBeta9Sandbox();
  const session = createSandboxSessionEnv(sandbox, "/workspace");
  const preAbort = new AbortController();
  const preAbortReason = new Error("stopped before dispatch");
  preAbort.abort(preAbortReason);

  await assert.rejects(
    session.exec("echo no", { signal: preAbort.signal }),
    (error: unknown) =>
      error instanceof DOMException &&
      error.name === "AbortError" &&
      error.cause === preAbortReason,
  );
  assert.equal(sandbox.execCalls, 0);

  const postAbort = new AbortController();
  sandbox.abortDuringExec = postAbort;
  await assert.rejects(
    session.exec("echo maybe", {
      cwd: "project",
      env: { NAME: "value" },
      timeoutMs: 321,
      signal: postAbort.signal,
    }),
    (error: unknown) =>
      error instanceof DOMException &&
      error.name === "AbortError" &&
      error.cause === "stopped after dispatch",
  );
  assert.equal(sandbox.execCalls, 1);
  assert.deepEqual(sandbox.lastExecOptions, {
    cwd: "/workspace/project",
    env: { NAME: "value" },
    timeoutMs: 321,
    signal: postAbort.signal,
  });
});

test("frozen Flue beta.9 provenance binds the compatibility source", async () => {
  const provenanceURL = new URL("./flue-runtime-beta9-source.json", import.meta.url);
  const compatibilityURL = new URL("./flue-runtime-beta9-compat.ts", import.meta.url);
  const licenseURL = new URL("./flue-runtime-beta9-LICENSE.txt", import.meta.url);
  const provenance = JSON.parse(await readFile(provenanceURL, "utf8")) as {
    package: {
      name: string;
      version: string;
      integrity: string;
      publishedAt: string;
    };
    upstream: {
      repository: string;
      tag: string;
      commit: string;
      license: string;
      licenseFile: { path: string; sha256: string };
      sourceFiles: ReadonlyArray<{ path: string; sha256: string }>;
    };
    compatibilityModule: { path: string; sha256: string };
  };
  const compatibilitySource = await readFile(compatibilityURL);
  const licenseSource = await readFile(licenseURL);

  assert.deepEqual(provenance.package, {
    name: "@flue/runtime",
    version: "1.0.0-beta.9",
    integrity: "sha512-ksh0ZkTVyqQnGvU3OnbVX6luAJwe6tt8q7O0vn99b7Cx6XcPTXzY/YEkXrOtCHzV6ZwfSdO9ZfaWbhTD1tdQuQ==",
    publishedAt: "2026-06-30T03:27:13.901Z",
  });
  assert.equal(provenance.upstream.repository, "https://github.com/withastro/flue");
  assert.equal(provenance.upstream.tag, "v1.0.0-beta.9");
  assert.equal(provenance.upstream.commit, "607d2613eb181a5e31c28a980847e101207d9fd3");
  assert.equal(provenance.upstream.license, "Apache-2.0");
  assert.deepEqual(provenance.upstream.licenseFile, {
    path: "LICENSE",
    sha256: "6acd933eaec32c0d8683f749c1232da0ecbc9e99d77e55c87be3da68d799183d",
  });
  assert.equal(
    createHash("sha256").update(licenseSource).digest("hex"),
    provenance.upstream.licenseFile.sha256,
  );
  assert.deepEqual(provenance.upstream.sourceFiles, [
    {
      path: "packages/runtime/src/abort.ts",
      sha256: "0fa5554959dfe4ce97fce072ea387b4af5105d36c637799a68721d5849bb08ff",
    },
    {
      path: "packages/runtime/src/sandbox.ts",
      sha256: "55fe1bed83b662285eef465bcd8275da4015a508f0faa86a3d6e041348569acb",
    },
    {
      path: "packages/runtime/src/types.ts",
      sha256: "5bcc9808c499d1760213889334de5abf7edfab46fba1375ab7bfeccd048c6204",
    },
  ]);
  assert.equal(provenance.compatibilityModule.path, "sdk/typescript/flue-runtime-beta9-compat.ts");
  assert.equal(
    createHash("sha256").update(compatibilitySource).digest("hex"),
    provenance.compatibilityModule.sha256,
  );
});

class RecordingFlueBeta9Sandbox implements SandboxApi {
  readonly calls: unknown[][] = [];
  failFirstWrite = false;
  execCalls = 0;
  abortDuringExec?: AbortController;
  lastExecOptions: unknown;

  async readFile(path: string): Promise<string> {
    this.calls.push(["readFile", path]);
    return "value";
  }

  async readFileBuffer(): Promise<Uint8Array> {
    return new Uint8Array([1]);
  }

  async writeFile(path: string, content: string | Uint8Array): Promise<void> {
    this.calls.push(["writeFile", path, content]);
    if (this.failFirstWrite) {
      this.failFirstWrite = false;
      throw new Error("missing parent");
    }
  }

  async stat(): Promise<FileStat> {
    return { isFile: true, isDirectory: false };
  }

  async readdir(): Promise<string[]> {
    return [];
  }

  async exists(): Promise<boolean> {
    return true;
  }

  async mkdir(path: string, options?: { recursive?: boolean }): Promise<void> {
    this.calls.push(["mkdir", path, options]);
  }

  async rm(
    path: string,
    options?: { recursive?: boolean; force?: boolean },
  ): Promise<void> {
    this.calls.push(["rm", path, options]);
  }

  async exec(
    _command: string,
    options?: {
      cwd?: string;
      env?: Record<string, string>;
      timeoutMs?: number;
      signal?: AbortSignal;
    },
  ): Promise<ShellResult> {
    this.execCalls++;
    this.lastExecOptions = options;
    this.abortDuringExec?.abort("stopped after dispatch");
    return { stdout: "", stderr: "", exitCode: 0 };
  }
}

import assert from "node:assert/strict";
import test from "node:test";

import type {
  ExecResult,
  SandboxFilesystem,
} from "./client.ts";
import {
  createSecondBoxFlueAdapter,
  SecondBoxFlueSandboxApi,
} from "./flue.ts";

test("Flue adapter translates its absolute root to workspace-relative paths", async () => {
  const filesystem = new RecordingSandbox();
  const api = new SecondBoxFlueSandboxApi(filesystem, {
    defaultDeadlineMilliseconds: 1_000,
    maximumOutputBytes: 16_384,
  });
  await api.writeFile("/workspace/nested/value.bin", new Uint8Array([0, 255]));
  assert.deepEqual(filesystem.writes, [["nested/value.bin", [0, 255]]]);
  await assert.rejects(api.readFile("/outside/value.txt"), /outside the SecondBox workspace root/);
  assert.equal(filesystem.reads.length, 0);
});

test("Flue adapter preserves stat fidelity and directory entry names", async () => {
  const filesystem = new RecordingSandbox();
  const api = new SecondBoxFlueSandboxApi(filesystem, {
    defaultDeadlineMilliseconds: 1_000,
    maximumOutputBytes: 16_384,
  });
  assert.deepEqual(await api.stat("/workspace/link"), {
    isFile: false,
    isDirectory: false,
    isSymbolicLink: true,
    size: 9,
    mtime: new Date("2026-07-28T00:00:00Z"),
  });
  assert.deepEqual(await api.readdir("/workspace/dir"), ["a.txt", "nested"]);
});

test("Flue adapter honors cwd, env, timeout, abort, and non-zero results", async () => {
  const filesystem = new RecordingSandbox();
  const api = new SecondBoxFlueSandboxApi(filesystem, {
    defaultDeadlineMilliseconds: 1_000,
    maximumOutputBytes: 16_384,
  });
  const controller = new AbortController();
  const result = await api.exec("exit 42", {
    cwd: "/workspace/project",
    env: { NAME: "value" },
    timeoutMs: 321,
    signal: controller.signal,
  });
  assert.deepEqual(filesystem.execOptions, {
    cwd: "project",
    environment: { NAME: "value" },
    deadlineMilliseconds: 321,
    maximumOutputBytes: 16_384,
    signal: controller.signal,
  });
  assert.deepEqual(result, { stdout: "out", stderr: "err", exitCode: 42 });
});

test("Flue adapter maps a structured deadline outcome to exit code 124", async () => {
  const filesystem = new RecordingSandbox();
  filesystem.execResult = {
    kind: "deadline_exceeded",
    elapsedMilliseconds: 321,
    stdout: new TextEncoder().encode("partial"),
    stderr: new TextEncoder().encode("warning"),
  };
  const api = new SecondBoxFlueSandboxApi(filesystem, {
    defaultDeadlineMilliseconds: 1_000,
    maximumOutputBytes: 16_384,
  });
  assert.deepEqual(await api.exec("sleep 10", { timeoutMs: 321 }), {
    stdout: "partial",
    stderr: "warning\nSecondBox command deadline exceeded after 321 ms",
    exitCode: 124,
  });
});

test("Flue session rejects an already-aborted command before transport mutation", async () => {
  const filesystem = new RecordingSandbox();
  const factory = createSecondBoxFlueAdapter(filesystem, {
    defaultDeadlineMilliseconds: 1_000,
    maximumOutputBytes: 16_384,
  });
  const session = await factory.createSessionEnv({ id: "workflow-aborted" });
  const controller = new AbortController();
  controller.abort(new Error("caller stopped"));
  await assert.rejects(
    session.exec("touch should-not-exist", { signal: controller.signal }),
    (error: unknown) => error instanceof DOMException && error.name === "AbortError",
  );
  assert.equal(filesystem.execCalls, 0);
});

test("Flue adapter forwards exact mkdir and rm option values", async () => {
  const filesystem = new RecordingSandbox();
  const api = new SecondBoxFlueSandboxApi(filesystem, {
    defaultDeadlineMilliseconds: 1_000,
    maximumOutputBytes: 16_384,
  });
  await api.mkdir("/workspace/a/b", { recursive: true });
  await api.rm("/workspace/a", { recursive: true, force: true });
  assert.deepEqual(filesystem.mkdirOptions, ["a/b", true]);
  assert.deepEqual(filesystem.removeOptions, ["a", true, true]);
});

test("separate Flue session environments retain files and never own lifecycle", async () => {
  const filesystem = new RecordingSandbox();
  const factory = createSecondBoxFlueAdapter(filesystem, {
    defaultDeadlineMilliseconds: 1_000,
    maximumOutputBytes: 16_384,
  });
  const first = await factory.createSessionEnv({ id: "workflow-1" });
  await first.writeFile("persisted.txt", "retained");
  await first.writeFile("binary.bin", new Uint8Array([0, 255, 1]));
  const second = await factory.createSessionEnv({ id: "workflow-2" });
  assert.equal(await second.readFile("persisted.txt"), "retained");
  assert.deepEqual(
    await second.readFileBuffer("binary.bin"),
    new Uint8Array([0, 255, 1]),
  );
  assert.equal(filesystem.lifecycleCalls, 0);
});

class RecordingSandbox implements SandboxFilesystem {
  readonly reads: string[] = [];
  readonly writes: Array<[string, number[]]> = [];
  readonly files = new Map<string, Uint8Array>();
  execOptions: unknown;
  execCalls = 0;
  execResult: ExecResult = {
    kind: "exited",
    exitCode: 42,
    stdout: new TextEncoder().encode("out"),
    stderr: new TextEncoder().encode("err"),
  };
  mkdirOptions: unknown;
  removeOptions: unknown;
  lifecycleCalls = 0;

  async readFile(path: string): Promise<Uint8Array> {
    this.reads.push(path);
    const value = this.files.get(path);
    if (value !== undefined) return value;
    return new TextEncoder().encode("text");
  }

  async writeFile(path: string, content: Uint8Array): Promise<void> {
    this.writes.push([path, Array.from(content)]);
    this.files.set(path, content);
  }

  async statFile(): Promise<{
    kind: "symbolic_link";
    sizeBytes: number;
    modifiedAt: string;
  }> {
    return {
      kind: "symbolic_link",
      sizeBytes: 9,
      modifiedAt: "2026-07-28T00:00:00Z",
    };
  }

  async listDirectory(): Promise<readonly { path: string }[]> {
    return [{ path: "dir/a.txt" }, { path: "dir/nested" }];
  }

  async fileExists(): Promise<boolean> {
    return true;
  }

  async createDirectory(path: string, recursive: boolean): Promise<void> {
    this.mkdirOptions = [path, recursive];
  }

  async removePath(
    path: string,
    recursive: boolean,
    force: boolean,
  ): Promise<void> {
    this.removeOptions = [path, recursive, force];
  }

  async exec(
    _command: string,
    options: unknown,
  ): Promise<ExecResult> {
    this.execCalls++;
    this.execOptions = options;
    return this.execResult;
  }
}

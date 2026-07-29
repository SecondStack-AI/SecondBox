import assert from "node:assert/strict";
import test from "node:test";

import {
  SandboxHandle,
  SecondBox,
} from "../../sdk/typescript/client.ts";
import {
  SecondBoxClient,
  type Sandbox,
} from "../../sdk/typescript/client.ts";
import { createSecondBoxFlueAdapter } from "../../sdk/typescript/flue.ts";

test("Flue complete command and filesystem subset uses the real SecondBox service contract", async () => {
  const baseURL = requiredEnvironment("SECONDBOX_FLUE_TEST_BASE_URL");
  const platformToken = requiredEnvironment("SECONDBOX_FLUE_TEST_PLATFORM_TOKEN");
  const tenantRef = requiredEnvironment("SECONDBOX_FLUE_TEST_TENANT_REF");
  const subjectRef = requiredEnvironment("SECONDBOX_FLUE_TEST_SUBJECT_REF");
  const sandboxID = requiredEnvironment("SECONDBOX_FLUE_TEST_SANDBOX_ID");
  const api = new SecondBox(
    new SecondBoxClient(baseURL, platformToken, fetch, tenantRef, subjectRef),
  );
  const sandbox = await api.requestJSON<Sandbox>("getSandbox", {
    pathParameters: { sandboxId: sandboxID },
  });
  const adapter = createSecondBoxFlueAdapter(
    new SandboxHandle(api, sandbox),
    {
      defaultDeadlineMilliseconds: 1_000,
      maximumOutputBytes: 4_096,
    },
  );

  const first = await adapter.createSessionEnv({ id: "flue-real-service-first" });
  await first.writeFile("nested/text.txt", "retained");
  await first.writeFile(
    "nested/binary.bin",
    new Uint8Array([0, 1, 0xfe, 0xff]),
  );

  const second = await adapter.createSessionEnv({ id: "flue-real-service-second" });
  assert.equal(await second.readFile("nested/text.txt"), "retained");
  assert.deepEqual(
    await second.readFileBuffer("nested/binary.bin"),
    new Uint8Array([0, 1, 0xfe, 0xff]),
  );
  assert.deepEqual((await second.readdir("nested")).sort(), [
    "binary.bin",
    "text.txt",
  ]);
  assert.equal(await second.exists("nested/text.txt"), true);

  const fileStat = await second.stat("nested/binary.bin");
  assert.equal(fileStat.isFile, true);
  assert.equal(fileStat.isDirectory, false);
  assert.equal(fileStat.isSymbolicLink, false);
  assert.equal(fileStat.size, 4);
  assert.equal(fileStat.mtime instanceof Date, true);
  const directoryStat = await second.stat("nested");
  assert.equal(directoryStat.isFile, false);
  assert.equal(directoryStat.isDirectory, true);

  await second.mkdir("nested/deeper", { recursive: true });
  const command = await second.exec("printf flue", {
    cwd: "nested",
    env: { FLUE_VALUE: "contract" },
    timeoutMs: 321,
  });
  assert.deepEqual(command, {
    stdout: "flue-out",
    stderr: "flue-err",
    exitCode: 17,
  });

  await second.rm("nested/binary.bin", {
    recursive: false,
    force: false,
  });
  assert.equal(await second.exists("nested/binary.bin"), false);
});

function requiredEnvironment(name: string): string {
  const value = process.env[name]?.trim();
  if (value === undefined || value === "") {
    throw new Error(`SecondBox real-service Flue test requires ${name}`);
  }
  return value;
}

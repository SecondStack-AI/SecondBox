import assert from "node:assert/strict";
import test from "node:test";

import {
  SecondBox,
  SecondBoxProblemError,
} from "../../sdk/typescript/client.ts";
import {
  SecondBoxClient,
  encodeJSONBody,
  type Operation,
  type Profile,
  type ProfileRevisionSpec,
  type Sandbox,
} from "../../sdk/typescript/client.ts";

const composeRunnerPoolName = "compose-live-pool";

test("TypeScript SDK live control-plane contract", async () => {
  const { application, profile } = await newTypeScriptLiveSubjectFixture();

  const operation = await application.requestJSON<Operation>("createSandbox", {
    headers: liveIdempotencyHeaders("typescript-create-sandbox"),
    body: encodeJSONBody({
      profile: profile.name,
      metadata: {
        sdk: "typescript",
        purpose: "live-contract",
      },
    }),
  });
  assert.notEqual(operation.id, "");
  assert.notEqual(operation.sandboxId, "");

  const sandbox = await application.requestJSON<Sandbox>("getSandbox", {
    pathParameters: { sandboxId: operation.sandboxId },
  });
  assert.equal(sandbox.metadata.sdk, "typescript");
  assert.equal(sandbox.profileRevisionId, profile.currentRevision.id);

  const handle = application.sandbox(sandbox);
  const refreshed = await handle.refresh();
  assert.equal(refreshed.id, sandbox.id);
  assert.equal(handle.snapshot.metadata.purpose, "live-contract");

  await assert.rejects(
    application.requestJSON<Sandbox>("getSandbox", {
      pathParameters: { sandboxId: "sbx_missing_live_contract" },
    }),
    (error: unknown) =>
      error instanceof SecondBoxProblemError &&
      error.status === 404 &&
      error.problem.code === "not_found" &&
      error.problem.requestId !== "",
  );
});

async function newTypeScriptLiveSubjectFixture(): Promise<{
  readonly application: SecondBox;
  readonly profile: Profile;
}> {
  const baseURL = requireLiveEnvironment("SECONDBOX_LIVE_BASE_URL");
  const platformToken = requireLiveEnvironment("SECONDBOX_LIVE_PLATFORM_TOKEN");
  const application = new SecondBox(new SecondBoxClient(
    baseURL,
    platformToken,
    fetch,
    "sdk-live-typescript",
    "sdk-live-typescript-subject",
  ));

  const profileName = "typescript-sdk-live";
  const profile = await application.requestJSON<Profile>("createProfile", {
    headers: liveIdempotencyHeaders("typescript-create-profile"),
    body: encodeJSONBody({
      name: profileName,
      spec: liveProfileRevisionSpec(),
    }),
  });
  assert.notEqual(profile.currentRevision.id, "");
  assert.equal(profile.name, profileName);

  return { application, profile };
}

function requireLiveEnvironment(name: string): string {
  const value = process.env[name]?.trim();
  if (value === undefined || value === "") {
    throw new Error(`SecondBox TypeScript live SDK test requires ${name}`);
  }
  return value;
}

function liveIdempotencyHeaders(value: string): Readonly<Record<string, string>> {
  return { "Idempotency-Key": value };
}

function liveProfileRevisionSpec(): ProfileRevisionSpec {
  return {
    pool: composeRunnerPoolName,
    architecture: "amd64",
    runtimeBundleDigest: `sha256:${"a".repeat(64)}`,
    toolchainBundleDigest: `sha256:${"b".repeat(64)}`,
    resources: {
      cpuMillis: 1000,
      memoryBytes: 2 ** 30,
      workspaceBytes: 8 * 2 ** 30,
      processLimit: 128,
      concurrentOperations: 4,
    },
    lifecycle: {
      initialState: "stopped",
      drainGraceSeconds: 30,
      idleSeconds: 300,
      maximumDurationSeconds: 3600,
      leaseSeconds: 60,
    },
    retention: {
      snapshotLimit: 8,
      snapshotRetentionSeconds: 86400,
      artifactRetentionSeconds: 86400,
    },
    execution: {
      maximumDeadlineMilliseconds: 60000,
      maximumBufferedOutputBytes: 2 ** 20,
      streamWindowBytes: 65536,
      maximumTransferBytes: 2 ** 30,
      terminalDetachSeconds: 30,
      dataPlaneTransport: "proxied",
    },
    network: {
      mode: "deny_all",
      destinations: [],
    },
    ports: [],
  };
}

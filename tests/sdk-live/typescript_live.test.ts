import assert from "node:assert/strict";
import test from "node:test";

import {
  SecondBox,
  SecondBoxProblemError,
} from "../../sdk/typescript/client.ts";
import {
  SecondBoxClient,
  encodeJSONBody,
  type CreateAPIKeyResponse,
  type Operation,
  type Profile,
  type ProfileRevisionSpec,
  type Project,
  type Sandbox,
  type ServiceAccount,
  type ServiceAccountScope,
} from "../../sdk/typescript/secondbox-client.gen.ts";

const composeRunnerPoolName = "compose-live-pool";

test("TypeScript SDK live control-plane contract", async () => {
  const baseURL = requireLiveEnvironment("SECONDBOX_LIVE_BASE_URL");
  const adminToken = requireLiveEnvironment("SECONDBOX_LIVE_ADMIN_TOKEN");
  const admin = new SecondBox(new SecondBoxClient(baseURL, adminToken, fetch));

  const project = await admin.requestJSON<Project>("createProject", {
    headers: liveIdempotencyHeaders("typescript-create-project"),
    body: encodeJSONBody({ name: "TypeScript SDK live project" }),
  });
  assert.notEqual(project.id, "");
  assert.equal(project.name, "TypeScript SDK live project");

  const profileName = "typescript-sdk-live";
  const profile = await admin.requestJSON<Profile>("createProfile", {
    headers: liveIdempotencyHeaders("typescript-create-profile"),
    body: encodeJSONBody({
      name: profileName,
      spec: liveProfileRevisionSpec(),
    }),
  });
  assert.notEqual(profile.currentRevision.id, "");
  assert.equal(profile.name, profileName);

  const scopes = [
    "sandbox:read",
    "sandbox:lifecycle",
  ] satisfies readonly ServiceAccountScope[];
  const account = await admin.requestJSON<ServiceAccount>("createServiceAccount", {
    pathParameters: { projectId: project.id },
    headers: liveIdempotencyHeaders("typescript-create-service-account"),
    body: encodeJSONBody({
      name: "typescript-sdk-live",
      scopes,
      profileGrants: [profileName],
    }),
  });
  assert.notEqual(account.id, "");
  assert.equal(account.projectId, project.id);

  const key = await admin.requestJSON<CreateAPIKeyResponse>("createAPIKey", {
    pathParameters: {
      projectId: project.id,
      serviceAccountId: account.id,
    },
    headers: liveIdempotencyHeaders("typescript-create-api-key"),
    body: encodeJSONBody({
      name: "typescript-sdk-live",
      scopes,
    }),
  });
  assert.notEqual(key.apiKey.id, "");
  assert.notEqual(key.credential, "");

  const application = new SecondBox(new SecondBoxClient(baseURL, key.credential, fetch));
  const operation = await application.requestJSON<Operation>("createSandbox", {
    headers: liveIdempotencyHeaders("typescript-create-sandbox"),
    body: encodeJSONBody({
      profile: profileName,
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
    backend: "firecracker",
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
    checkpoint: {
      onStop: false,
      retentionSeconds: 86400,
      snapshotLimit: 8,
      artifactRetentionSeconds: 86400,
    },
    execution: {
      maximumDeadlineMilliseconds: 60000,
      maximumBufferedOutputBytes: 2 ** 20,
      streamWindowBytes: 65536,
      maximumTransferBytes: 2 ** 30,
      terminalDetachSeconds: 30,
    },
    network: {
      mode: "deny_all",
      destinations: [],
    },
    ports: [],
  };
}

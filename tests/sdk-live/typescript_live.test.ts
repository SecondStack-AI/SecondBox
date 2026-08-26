import assert from "node:assert/strict";
import test from "node:test";

import {
  SecondBox,
  SecondBoxProblemError,
} from "../../sdk/typescript/client.ts";
import {
  SecondBoxClient,
  type Profile,
  type Sandbox,
} from "../../sdk/typescript/client.ts";

test("TypeScript SDK live control-plane contract", async () => {
  const { application, profile } = await newTypeScriptLiveSubjectFixture();

  const { handle, operation } = await application.createSandbox({
    profile: profile.name,
    metadata: { sdk: "typescript", purpose: "live-contract" },
    idempotencyKey: "typescript-create-sandbox",
  });
  assert.notEqual(operation.id, "");
  assert.notEqual(operation.sandboxId, "");

  const sandbox = handle.snapshot;
  assert.equal(sandbox.metadata.sdk, "typescript");
  assert.equal(sandbox.profileRevisionId, profile.currentRevision.id);

  const page = await application.listSandboxes({
    metadata: { sdk: "typescript", purpose: "live-contract" },
  });
  assert.equal(page.items.length, 1);
  assert.equal(page.items[0]?.id, sandbox.id);
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
  const applicationToken = requireLiveEnvironment("SECONDBOX_LIVE_APPLICATION_TOKEN");
  const tenantRef = requireLiveEnvironment("SECONDBOX_LIVE_TENANT_REF");
  const subjectRef = requireLiveEnvironment("SECONDBOX_LIVE_SUBJECT_REF");
  const application = new SecondBox(new SecondBoxClient(
    baseURL,
    applicationToken,
    fetch,
    tenantRef,
    subjectRef,
  ));

  const profileName = "typescript-sdk-live";
  const profile = await application.getProfile(profileName);
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

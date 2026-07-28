#!/usr/bin/env node

import { writeFileSync } from "node:fs";
import path from "node:path";

if (process.argv.length !== 5) {
  console.error(
    "Usage: scripts/write-protected-release-workflow-identity.mjs KIND OUTPUT.json PROTECTED_ENVIRONMENT",
  );
  process.exit(2);
}

const workflowDefinitions = new Map([
  [
    "candidate",
    {
      path: ".github/workflows/release-candidate.yml",
      jobName: "assemble-release-candidate",
    },
  ],
  [
    "qualification",
    {
      path: ".github/workflows/release-qualification.yml",
      jobName: "qualify-packaged-release",
    },
  ],
]);

const kind = process.argv[2];
const outputPath = path.resolve(process.argv[3]);
const protectedEnvironment = process.argv[4];
const definition = workflowDefinitions.get(kind);
if (definition === undefined) {
  fail(`unknown protected release workflow kind: ${kind}`);
}

const identity = {
  schemaVersion: 1,
  kind,
  repository: requireEnvironment("GITHUB_REPOSITORY"),
  workflowPath: definition.path,
  workflowRef: requireEnvironment("GITHUB_WORKFLOW_REF"),
  protectedEnvironment,
  sourceCommit: requireEnvironment("GITHUB_SHA"),
  ref: requireEnvironment("GITHUB_REF"),
  eventName: requireEnvironment("GITHUB_EVENT_NAME"),
  runID: parsePositiveInteger("GITHUB_RUN_ID"),
  runAttempt: parsePositiveInteger("GITHUB_RUN_ATTEMPT"),
  jobName: definition.jobName,
  runnerName: requireEnvironment("RUNNER_NAME"),
  runnerEnvironment: requireEnvironment("RUNNER_ENVIRONMENT"),
  runnerOS: requireEnvironment("RUNNER_OS"),
  runnerArch: requireEnvironment("RUNNER_ARCH"),
};

writeFileSync(outputPath, `${JSON.stringify(identity, null, 2)}\n`, {
  encoding: "utf8",
  flag: "wx",
  mode: 0o644,
});

function requireEnvironment(name) {
  const value = process.env[name];
  if (value === undefined || value.length === 0) {
    fail(`set ${name}`);
  }
  return value;
}

function parsePositiveInteger(name) {
  const value = requireEnvironment(name);
  if (!/^[1-9][0-9]*$/.test(value) || !Number.isSafeInteger(Number(value))) {
    fail(`${name} must be a positive safe integer`);
  }
  return Number(value);
}

function fail(message) {
  console.error(`SecondBox protected release workflow identity failed: ${message}`);
  process.exit(1);
}

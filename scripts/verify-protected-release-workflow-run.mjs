#!/usr/bin/env node

import { readFileSync } from "node:fs";

if (process.argv.length !== 7) {
  console.error(
    "Usage: scripts/verify-protected-release-workflow-run.mjs RUN.json JOBS.json IDENTITY.json KIND SOURCE_COMMIT",
  );
  process.exit(2);
}

const expectedRepository = "SecondStack-AI/SecondBox";
const expectedBranch = "main";
const workflowDefinitions = new Map([
  [
    "candidate",
    {
      path: ".github/workflows/release-candidate.yml",
      environment: "release-candidate",
      jobName: "assemble-release-candidate",
      runnerGroup: "secondbox-release",
      requiredRunnerLabels: [
        "self-hosted",
        "linux",
        "x64",
        "secondbox-release-builder",
      ],
    },
  ],
  [
    "qualification",
    {
      path: ".github/workflows/release-qualification.yml",
      environment: "release-qualification",
      jobName: "qualify-packaged-release",
      runnerGroup: "secondbox-release",
      requiredRunnerLabels: [
        "self-hosted",
        "linux",
        "x64",
        "secondbox-release-qualification",
      ],
    },
  ],
]);

const run = readJSON(process.argv[2], "workflow run");
const jobs = readJSON(process.argv[3], "workflow jobs");
const identity = readJSON(process.argv[4], "protected workflow identity");
const kind = process.argv[5];
const sourceCommit = process.argv[6];
const definition = workflowDefinitions.get(kind);
if (definition === undefined) {
  fail(`unknown protected workflow kind: ${kind}`);
}
if (!/^[0-9a-f]{40}$/.test(sourceCommit)) {
  fail("expected source commit must be a full lowercase commit SHA");
}

verifyCanonicalWorkflowRun(run, definition, sourceCommit);
const job = verifyCanonicalWorkflowJob(jobs, definition);
verifyProtectedWorkflowIdentity(identity, run, job, definition, kind, sourceCommit);
console.log(
  `SecondBox verified protected ${kind} workflow run ${run.id} at ${sourceCommit}`,
);

function verifyCanonicalWorkflowRun(runDocument, expected, expectedCommit) {
  const workflowPath = String(runDocument.path ?? "").split("@", 1)[0];
  if (
    runDocument.repository?.full_name !== expectedRepository ||
    runDocument.head_repository?.full_name !== expectedRepository
  ) {
    fail(`workflow run repository must be ${expectedRepository}`);
  }
  if (
    workflowPath !== expected.path ||
    runDocument.event !== "workflow_dispatch" ||
    runDocument.head_branch !== expectedBranch ||
    runDocument.head_sha !== expectedCommit ||
    runDocument.status !== "completed" ||
    runDocument.conclusion !== "success"
  ) {
    fail(
      `workflow run must be a successful ${expected.path} dispatch from protected ${expectedBranch} at the exact candidate commit`,
    );
  }
  if (!Number.isSafeInteger(runDocument.id) || runDocument.id < 1) {
    fail("workflow run id must be a positive integer");
  }
  if (!Number.isSafeInteger(runDocument.run_attempt) || runDocument.run_attempt < 1) {
    fail("workflow run attempt must be a positive integer");
  }
}

function verifyCanonicalWorkflowJob(jobDocument, expected) {
  if (
    !Number.isSafeInteger(jobDocument.total_count) ||
    !Array.isArray(jobDocument.jobs)
  ) {
    fail("workflow jobs response is malformed");
  }
  const matchingJobs = jobDocument.jobs.filter(
    (candidate) => candidate.name === expected.jobName,
  );
  if (matchingJobs.length !== 1) {
    fail(`workflow run must contain exactly one ${expected.jobName} job`);
  }
  const job = matchingJobs[0];
  if (
    job.status !== "completed" ||
    job.conclusion !== "success" ||
    job.runner_group_name !== expected.runnerGroup ||
    typeof job.runner_name !== "string" ||
    job.runner_name.length === 0 ||
    !Array.isArray(job.labels) ||
    job.labels.some((label) => typeof label !== "string")
  ) {
    fail(
      `${expected.jobName} must succeed on runner group ${expected.runnerGroup}`,
    );
  }
  const normalizedLabels = new Set(
    job.labels.map((label) => label.toLowerCase()),
  );
  for (const label of expected.requiredRunnerLabels) {
    if (!normalizedLabels.has(label)) {
      fail(`${expected.jobName} runner is missing required label ${label}`);
    }
  }
  return job;
}

function verifyProtectedWorkflowIdentity(
  identityDocument,
  runDocument,
  job,
  expected,
  expectedKind,
  expectedCommit,
) {
  const allowedKeys = new Set([
    "schemaVersion",
    "kind",
    "repository",
    "workflowPath",
    "workflowRef",
    "protectedEnvironment",
    "sourceCommit",
    "ref",
    "eventName",
    "runID",
    "runAttempt",
    "jobName",
    "runnerName",
    "runnerEnvironment",
    "runnerOS",
    "runnerArch",
  ]);
  const actualKeys = Object.keys(identityDocument);
  if (
    actualKeys.some((key) => !allowedKeys.has(key)) ||
    actualKeys.length !== allowedKeys.size
  ) {
    fail("protected workflow identity has missing or unexpected fields");
  }
  const expectedWorkflowRefPrefix = `${expectedRepository}/${expected.path}@`;
  if (
    identityDocument.schemaVersion !== 1 ||
    identityDocument.kind !== expectedKind ||
    identityDocument.repository !== expectedRepository ||
    identityDocument.workflowPath !== expected.path ||
    !String(identityDocument.workflowRef ?? "").startsWith(
      expectedWorkflowRefPrefix,
    ) ||
    identityDocument.protectedEnvironment !== expected.environment ||
    identityDocument.sourceCommit !== expectedCommit ||
    identityDocument.ref !== "refs/heads/main" ||
    identityDocument.eventName !== "workflow_dispatch" ||
    identityDocument.runID !== runDocument.id ||
    identityDocument.runAttempt !== runDocument.run_attempt ||
    identityDocument.jobName !== expected.jobName ||
    identityDocument.runnerName !== job.runner_name ||
    identityDocument.runnerEnvironment !== "self-hosted" ||
    identityDocument.runnerOS !== "Linux" ||
    identityDocument.runnerArch !== "X64"
  ) {
    fail(
      `protected workflow identity does not match the canonical run, ${expected.environment} environment, or qualified host`,
    );
  }
}

function readJSON(documentPath, label) {
  try {
    return JSON.parse(readFileSync(documentPath, "utf8"));
  } catch (error) {
    fail(`${label} could not be decoded: ${error.message}`);
  }
}

function fail(message) {
  console.error(`SecondBox protected release workflow run invalid: ${message}`);
  process.exit(1);
}

#!/usr/bin/env node

import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

if (process.argv.length !== 11) {
  console.error(
    "Usage: scripts/generate-multirunner-qualification-record.mjs SUBJECTS.json EVIDENCE_DIRECTORY RECORD.json STARTED_AT COMPLETED_AT HOSTS.json HOST_A_ID HOST_B_ID CONTROLLER_ID",
  );
  process.exit(2);
}

const subjectManifestPath = secureFile(process.argv[2], "release subject manifest");
const evidenceDirectory = secureDirectory(process.argv[3], "qualification evidence directory");
const recordPath = secureNewFile(process.argv[4], evidenceDirectory, "multi-Runner record");
const startedAt = process.argv[5];
const completedAt = process.argv[6];
const hostsPath = secureFile(process.argv[7], "qualification hosts");
const selectedHostIDs = [process.argv[10], process.argv[8], process.argv[9]];
const subjectManifestContents = fs.readFileSync(subjectManifestPath);
const subjectManifest = JSON.parse(subjectManifestContents);
const hostInventory = JSON.parse(fs.readFileSync(hostsPath, "utf8"));
const requirements = JSON.parse(
  fs.readFileSync(
    path.join(
      path.dirname(fileURLToPath(import.meta.url)),
      "..",
      "release",
      "qualification-requirements.json",
    ),
    "utf8",
  ),
);
const scenarioIDs = requirements.gates["multi-runner"].scenarioIds;

if (
  subjectManifest.status !== "passed" ||
  !/^[0-9a-f]{40}$/.test(subjectManifest.sourceCommit ?? "") ||
  !Array.isArray(subjectManifest.subjects) ||
  subjectManifest.subjects.length !== 13
) {
  fail("release subject manifest is not a passing canonical candidate");
}
if (!Array.isArray(hostInventory.hosts) || hostInventory.hosts.length < 3) {
  fail("qualification hosts must contain the controller and two Runner hosts");
}
if (new Set(selectedHostIDs).size !== 3) {
  fail("selected qualification host identities must be distinct");
}
const hosts = selectedHostIDs.map((hostID, index) => {
  const matches = hostInventory.hosts.filter((host) => host.id === hostID);
  if (matches.length !== 1) {
    fail(`selected qualification host ${hostID} must appear exactly once`);
  }
  const host = matches[0];
  if (
    (index === 0 && host.role !== "controller") ||
    (index > 0 &&
      (host.role !== "runner" || host.dedicated !== true || host.kvm !== true))
  ) {
    fail(`selected qualification host ${hostID} has the wrong role or capability`);
  }
  return host;
});

const subjectManifestSHA256 = sha256(subjectManifestContents);
const scenarios = scenarioIDs.map((scenarioID) => {
  const relativePath = `qualification/multi-runner/${scenarioID}.json`;
  const artifactPath = secureFile(
    path.join(evidenceDirectory, ...relativePath.split("/")),
    `scenario ${scenarioID}`,
  );
  const artifactContents = fs.readFileSync(artifactPath);
  const artifact = JSON.parse(artifactContents);
  if (
    artifact.schemaVersion !== 1 ||
    artifact.scenarioId !== scenarioID ||
    artifact.status !== "passed" ||
    artifact.sourceCommit !== subjectManifest.sourceCommit ||
    artifact.subjectManifestSHA256 !== subjectManifestSHA256
  ) {
    fail(`scenario artifact ${scenarioID} is not bound to the candidate`);
  }
  return {
    id: scenarioID,
    status: "passed",
    summary: artifact.summary,
    artifacts: [{ path: relativePath, sha256: sha256(artifactContents) }],
  };
});

const record = {
  schemaVersion: 1,
  gate: "multi-runner",
  releaseVersion: subjectManifest.releaseVersion,
  sourceCommit: subjectManifest.sourceCommit,
  subjectManifestSHA256,
  startedAt,
  completedAt,
  status: "passed",
  summary: "Two explicit qualified KVM Runner hosts passed placement, drain, relocation, crash, stale-message, and generation-fencing qualification.",
  hosts,
  subjects: subjectManifest.subjects.map((subject) => ({
    id: subject.id,
    kind: subject.kind,
    locator: subject.locator,
    sha256: subject.digest.sha256,
  })),
  scenarios,
};
fs.writeFileSync(recordPath, `${JSON.stringify(record, null, 2)}\n`, {
  encoding: "utf8",
  flag: "wx",
  mode: 0o600,
});

function sha256(contents) {
  return createHash("sha256").update(contents).digest("hex");
}

function secureDirectory(directoryPath, label) {
  const resolvedPath = path.resolve(directoryPath);
  const status = fs.lstatSync(resolvedPath);
  const canonicalPath = fs.realpathSync(resolvedPath);
  if (!status.isDirectory() || status.isSymbolicLink() || canonicalPath !== resolvedPath) {
    fail(`${label} must be a canonical non-symbolic-link directory`);
  }
  return canonicalPath;
}

function secureFile(filePath, label) {
  const resolvedPath = path.resolve(filePath);
  const status = fs.lstatSync(resolvedPath);
  const canonicalPath = fs.realpathSync(resolvedPath);
  if (!status.isFile() || status.isSymbolicLink() || canonicalPath !== resolvedPath) {
    fail(`${label} must be a canonical regular non-symbolic-link file`);
  }
  return canonicalPath;
}

function secureNewFile(filePath, rootDirectory, label) {
  const resolvedPath = path.resolve(filePath);
  if (!resolvedPath.startsWith(`${rootDirectory}${path.sep}`)) {
    fail(`${label} must remain inside the evidence directory`);
  }
  if (fs.existsSync(resolvedPath)) {
    fail(`${label} already exists`);
  }
  return resolvedPath;
}

function fail(message) {
  console.error(`SecondBox multi-Runner qualification record generation failed: ${message}`);
  process.exit(1);
}

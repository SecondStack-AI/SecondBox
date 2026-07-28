#!/usr/bin/env node

import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";

if (process.argv.length !== 6) {
  console.error(
    "Usage: scripts/verify-release-qualification-record.mjs SUBJECTS.json RECORD.json EXPECTED_GATE EVIDENCE_DIRECTORY",
  );
  process.exit(2);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(scriptDirectory, "..");
const subjectManifestPath = path.resolve(process.argv[2]);
const recordPath = path.resolve(process.argv[3]);
const expectedGate = process.argv[4];
const evidenceDirectory = secureDirectory(process.argv[5], "evidence directory");
const subjectManifestContents = secureFileContents(
  subjectManifestPath,
  "release subject manifest",
);
const recordContents = secureEvidenceFileContents(
  recordPath,
  "qualification record",
);
const subjectManifest = decodeJSON(
  subjectManifestContents,
  "release subject manifest",
);
const record = decodeJSON(recordContents, "qualification record");
const requirements = readRepositoryJSON(
  "release/qualification-requirements.json",
  "qualification requirements",
);

validateDocument(
  compileRepositorySchema("release/supply-chain-subjects-schema.json"),
  subjectManifest,
  "release subject manifest",
);
validateDocument(
  compileRepositorySchema("release/qualification-record-schema.json"),
  record,
  "qualification record",
);
validateRequirements(requirements);

const gateRequirements = requirements.gates[expectedGate];
if (gateRequirements === undefined) {
  fail(`SecondBox qualification expected gate is unknown: ${expectedGate}`);
}
if (record.gate !== expectedGate) {
  fail(
    `SecondBox qualification record gate ${record.gate} does not match ${expectedGate}`,
  );
}
if (
  record.releaseVersion !== subjectManifest.releaseVersion ||
  record.sourceCommit !== subjectManifest.sourceCommit
) {
  fail("SecondBox qualification record candidate identity mismatch");
}
const subjectManifestSHA256 = sha256(subjectManifestContents);
if (record.subjectManifestSHA256 !== subjectManifestSHA256) {
  fail("SecondBox qualification record subject manifest digest mismatch");
}
if (subjectManifest.status !== "passed") {
  fail(
    `SecondBox qualification subject manifest status is ${subjectManifest.status}`,
  );
}
if (record.status !== "passed") {
  fail(`SecondBox qualification record status is ${record.status}`);
}
if (Date.parse(record.completedAt) < Date.parse(record.startedAt)) {
  fail("SecondBox qualification record completedAt precedes startedAt");
}

const manifestSubjects = indexUnique(
  subjectManifest.subjects,
  "id",
  "release subject manifest",
);
const recordSubjects = indexUnique(
  record.subjects,
  "id",
  "qualification record",
);
if (recordSubjects.size !== manifestSubjects.size) {
  fail(
    `SecondBox qualification record subject count ${recordSubjects.size} does not match manifest count ${manifestSubjects.size}`,
  );
}
for (const [subjectID, manifestSubject] of manifestSubjects) {
  if (manifestSubject.status !== "passed") {
    fail(`SecondBox release subject ${subjectID} status is ${manifestSubject.status}`);
  }
  const recordSubject = recordSubjects.get(subjectID);
  if (recordSubject === undefined) {
    fail(`SecondBox qualification record is missing subject ${subjectID}`);
  }
  if (recordSubject.kind !== manifestSubject.kind) {
    fail(`SecondBox qualification record kind mismatch for subject ${subjectID}`);
  }
  if (recordSubject.locator !== manifestSubject.locator) {
    fail(`SecondBox qualification record locator mismatch for subject ${subjectID}`);
  }
  if (recordSubject.sha256 !== manifestSubject.digest.sha256) {
    fail(`SecondBox qualification record digest mismatch for subject ${subjectID}`);
  }
}
for (const subjectID of recordSubjects.keys()) {
  if (!manifestSubjects.has(subjectID)) {
    fail(`SecondBox qualification record has extra subject ${subjectID}`);
  }
}

const hosts = indexUnique(record.hosts, "id", "qualification record hosts");
const qualifiedRunnerHosts = [...hosts.values()].filter(
  (host) =>
    host.role === "runner" &&
    host.operatingSystem === "linux" &&
    host.architecture === "amd64" &&
    host.dedicated === true &&
    host.kvm === true,
);
if (
  qualifiedRunnerHosts.length <
  gateRequirements.minimumDedicatedKVMRunnerHosts
) {
  fail(
    `SecondBox qualification gate ${expectedGate} requires at least ${gateRequirements.minimumDedicatedKVMRunnerHosts} dedicated KVM runner hosts`,
  );
}
for (const deploymentMode of gateRequirements.requiredRunnerDeploymentModes) {
  if (
    !qualifiedRunnerHosts.some(
      (host) => host.deploymentMode === deploymentMode,
    )
  ) {
    fail(
      `SecondBox qualification gate ${expectedGate} requires a dedicated KVM runner deployed with ${deploymentMode}`,
    );
  }
}

const scenarios = indexUnique(
  record.scenarios,
  "id",
  "qualification record scenarios",
);
const requiredScenarioIDs = new Set(gateRequirements.scenarioIds);
if (scenarios.size !== requiredScenarioIDs.size) {
  fail(
    `SecondBox qualification gate ${expectedGate} scenario count ${scenarios.size} does not match required count ${requiredScenarioIDs.size}`,
  );
}
for (const scenarioID of requiredScenarioIDs) {
  const scenario = scenarios.get(scenarioID);
  if (scenario === undefined) {
    fail(
      `SecondBox qualification gate ${expectedGate} is missing scenario ${scenarioID}`,
    );
  }
  if (scenario.status !== "passed") {
    fail(
      `SecondBox qualification scenario ${scenarioID} status is ${scenario.status}`,
    );
  }
  for (const artifact of scenario.artifacts) {
    verifyArtifact(artifact, scenarioID);
  }
}
for (const scenarioID of scenarios.keys()) {
  if (!requiredScenarioIDs.has(scenarioID)) {
    fail(
      `SecondBox qualification gate ${expectedGate} has unexpected scenario ${scenarioID}`,
    );
  }
}

console.log(
  `SecondBox ${expectedGate} qualification record passed for ${subjectManifest.sourceCommit}`,
);

function fail(message) {
  console.error(message);
  process.exit(1);
}

function decodeJSON(contents, label) {
  try {
    return JSON.parse(contents);
  } catch (error) {
    fail(`SecondBox ${label} is not valid JSON: ${error.message}`);
  }
}

function readRepositoryJSON(relativePath, label) {
  const documentPath = path.join(repositoryRoot, relativePath);
  return decodeJSON(
    secureFileContents(documentPath, label),
    label,
  );
}

function compileRepositorySchema(relativePath) {
  const schema = readRepositoryJSON(relativePath, `${relativePath} schema`);
  const ajv = new Ajv2020({
    allErrors: true,
    strict: true,
    formats: {
      "date-time": {
        type: "string",
        validate: (value) =>
          /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/.test(value) &&
          !Number.isNaN(Date.parse(value)),
      },
    },
  });
  return ajv.compile(schema);
}

function validateDocument(validate, document, label) {
  if (validate(document)) {
    return;
  }
  const details = validate.errors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
  fail(`SecondBox ${label} schema validation failed: ${details}`);
}

function validateRequirements(document) {
  if (
    document === null ||
    typeof document !== "object" ||
    Array.isArray(document) ||
    document.schemaVersion !== 1 ||
    document.gates === null ||
    typeof document.gates !== "object" ||
    Array.isArray(document.gates)
  ) {
    fail("SecondBox qualification requirements are malformed");
  }
  const schema = readRepositoryJSON(
    "release/qualification-record-schema.json",
    "qualification record schema",
  );
  const schemaGates = schema.properties.gate.enum;
  const requirementGates = Object.keys(document.gates);
  if (
    requirementGates.length !== schemaGates.length ||
    schemaGates.some((gate) => !requirementGates.includes(gate))
  ) {
    fail(
      "SecondBox qualification requirements do not exactly cover the record gate enum",
    );
  }
  for (const gate of schemaGates) {
    const requirement = document.gates[gate];
    const keys = Object.keys(requirement).sort();
    if (
      JSON.stringify(keys) !==
      JSON.stringify(
        [
          "minimumDedicatedKVMRunnerHosts",
          "requiredRunnerDeploymentModes",
          "scenarioIds",
        ].sort(),
      )
    ) {
      fail(`SecondBox qualification requirements for ${gate} are malformed`);
    }
    if (
      !Number.isInteger(requirement.minimumDedicatedKVMRunnerHosts) ||
      requirement.minimumDedicatedKVMRunnerHosts < 1
    ) {
      fail(
        `SecondBox qualification requirements for ${gate} have invalid host count`,
      );
    }
    if (
      !Array.isArray(requirement.requiredRunnerDeploymentModes) ||
      requirement.requiredRunnerDeploymentModes.some(
        (mode) => mode !== "systemd" && mode !== "compose",
      ) ||
      new Set(requirement.requiredRunnerDeploymentModes).size !==
        requirement.requiredRunnerDeploymentModes.length
    ) {
      fail(
        `SecondBox qualification requirements for ${gate} have invalid deployment modes`,
      );
    }
    if (
      !Array.isArray(requirement.scenarioIds) ||
      requirement.scenarioIds.length === 0 ||
      requirement.scenarioIds.some(
        (scenarioID) =>
          typeof scenarioID !== "string" ||
          !/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(scenarioID),
      ) ||
      new Set(requirement.scenarioIds).size !== requirement.scenarioIds.length
    ) {
      fail(
        `SecondBox qualification requirements for ${gate} have invalid scenario IDs`,
      );
    }
  }
}

function secureDirectory(directoryPath, label) {
  const resolvedPath = path.resolve(directoryPath);
  let fileLstat;
  let canonicalPath;
  try {
    fileLstat = fs.lstatSync(resolvedPath);
    canonicalPath = fs.realpathSync(resolvedPath);
  } catch (error) {
    fail(`SecondBox ${label} is unavailable: ${error.message}`);
  }
  if (
    !fileLstat.isDirectory() ||
    fileLstat.isSymbolicLink() ||
    canonicalPath !== resolvedPath
  ) {
    fail(`SecondBox ${label} must be a canonical non-symbolic-link directory`);
  }
  return canonicalPath;
}

function secureFileContents(filePath, label) {
  const resolvedPath = path.resolve(filePath);
  let fileLstat;
  let canonicalPath;
  try {
    fileLstat = fs.lstatSync(resolvedPath);
    canonicalPath = fs.realpathSync(resolvedPath);
  } catch (error) {
    fail(`SecondBox ${label} is unavailable: ${error.message}`);
  }
  if (
    !fileLstat.isFile() ||
    fileLstat.isSymbolicLink() ||
    canonicalPath !== resolvedPath
  ) {
    fail(`SecondBox ${label} must be a canonical regular non-symbolic-link file`);
  }
  return fs.readFileSync(canonicalPath);
}

function secureEvidenceFileContents(filePath, label) {
  const resolvedPath = path.resolve(filePath);
  if (!resolvedPath.startsWith(`${evidenceDirectory}${path.sep}`)) {
    fail(`SecondBox ${label} is outside the evidence directory`);
  }
  return secureFileContents(resolvedPath, label);
}

function indexUnique(entries, idKey, label) {
  const indexed = new Map();
  for (const entry of entries) {
    const entryID = entry[idKey];
    if (indexed.has(entryID)) {
      fail(`SecondBox ${label} has duplicate ${idKey} ${entryID}`);
    }
    indexed.set(entryID, entry);
  }
  return indexed;
}

function verifyArtifact(artifact, scenarioID) {
  const normalizedPath = path.posix.normalize(artifact.path);
  if (
    path.posix.isAbsolute(artifact.path) ||
    normalizedPath !== artifact.path ||
    normalizedPath.startsWith("../") ||
    normalizedPath.includes("/../")
  ) {
    fail(
      `SecondBox qualification scenario ${scenarioID} has unsafe artifact path ${artifact.path}`,
    );
  }
  const artifactPath = path.join(
    evidenceDirectory,
    ...artifact.path.split("/"),
  );
  const contents = secureEvidenceFileContents(
    artifactPath,
    `qualification artifact ${artifact.path}`,
  );
  if (sha256(contents) !== artifact.sha256) {
    fail(
      `SecondBox qualification artifact digest mismatch for ${artifact.path}`,
    );
  }
}

function sha256(contents) {
  return createHash("sha256").update(contents).digest("hex");
}

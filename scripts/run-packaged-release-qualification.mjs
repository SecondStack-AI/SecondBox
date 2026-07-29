#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  closeSync,
  lstatSync,
  mkdirSync,
  openSync,
  readFileSync,
  readdirSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";

if (process.argv.length < 8) {
  console.error(
    "Usage: scripts/run-packaged-release-qualification.mjs CANDIDATE_DIRECTORY SUBJECTS.json HOSTS.json CONTROLLER_DIRECTORY OUTPUT_DIRECTORY GATE [GATE...]",
  );
  process.exit(2);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(scriptDirectory, "..");
const candidateDirectory = secureDirectory(
  process.argv[2],
  "candidate directory",
);
const subjectManifestPath = secureFile(
  process.argv[3],
  "release subject manifest",
);
const hostInventoryPath = secureFile(
  process.argv[4],
  "qualified host inventory",
);
const controllerDirectory = secureDirectory(
  process.argv[5],
  "scenario controller directory",
);
const outputDirectory = secureDirectory(
  process.argv[6],
  "qualification output directory",
);
const requestedGates = process.argv.slice(7);
const supportedGates = new Set([
  "kvm",
  "durability",
  "data-plane",
  "network",
  "security",
]);

if (
  requestedGates.length === 0 ||
  new Set(requestedGates).size !== requestedGates.length ||
  requestedGates.some((gate) => !supportedGates.has(gate))
) {
  fail(
    "requested gates must be a non-empty unique subset of kvm, durability, data-plane, network, and security",
  );
}
if (
  subjectManifestPath !==
  path.join(candidateDirectory, "release-subjects.json")
) {
  fail(
    "release subject manifest must be the canonical release-subjects.json inside the candidate directory",
  );
}
if (readdirSync(outputDirectory).length !== 0) {
  fail("qualification output directory must be empty");
}

const subjectManifestContents = readFileSync(subjectManifestPath);
const subjectManifestSHA256 = sha256(subjectManifestContents);
const hostInventoryContents = readFileSync(hostInventoryPath);
const hostInventorySHA256 = sha256(hostInventoryContents);
const subjectManifest = decodeJSON(
  subjectManifestContents,
  "release subject manifest",
);
const hostInventory = decodeJSON(
  hostInventoryContents,
  "qualified host inventory",
);
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
  compileRepositorySchema("release/qualification-hosts-schema.json"),
  hostInventory,
  "qualified host inventory",
);
if (subjectManifest.status !== "passed") {
  fail(
    `release subject manifest status is ${String(subjectManifest.status)}`,
  );
}

const subjects = indexUnique(
  subjectManifest.subjects,
  "id",
  "release subject manifest",
);
for (const subject of subjects.values()) {
  verifyCandidateSubject(subject);
}
const hosts = indexUnique(
  hostInventory.hosts,
  "id",
  "qualified host inventory",
);
const qualifiedRunnerHosts = [...hosts.values()].filter(isQualifiedRunnerHost);
const scenarioControllers = new Map();
for (const gate of requestedGates) {
  const gateRequirements = requirements.gates?.[gate];
  if (
    gateRequirements === undefined ||
    !Array.isArray(gateRequirements.scenarioIds) ||
    !Number.isInteger(gateRequirements.minimumDedicatedKVMRunnerHosts) ||
    !Array.isArray(gateRequirements.requiredRunnerDeploymentModes)
  ) {
    fail(`qualification requirements for ${gate} are malformed`);
  }
  if (
    qualifiedRunnerHosts.length <
    gateRequirements.minimumDedicatedKVMRunnerHosts
  ) {
    fail(
      `gate ${gate} requires at least ${gateRequirements.minimumDedicatedKVMRunnerHosts} dedicated KVM runner hosts`,
    );
  }
  for (const deploymentMode of gateRequirements.requiredRunnerDeploymentModes) {
    if (
      !qualifiedRunnerHosts.some(
        (host) => host.deploymentMode === deploymentMode,
      )
    ) {
      fail(
        `gate ${gate} requires a dedicated KVM runner deployed with ${deploymentMode}`,
      );
    }
  }
  const gateControllerDirectory = secureDirectory(
    path.join(controllerDirectory, gate),
    `${gate} scenario controller directory`,
  );
  const requiredControllerNames = [...gateRequirements.scenarioIds].sort();
  const actualControllerNames = readdirSync(
    gateControllerDirectory,
  ).sort();
  if (
    actualControllerNames.length !== requiredControllerNames.length ||
    actualControllerNames.some(
      (name, index) => name !== requiredControllerNames[index],
    )
  ) {
    fail(
      `${gate} scenario controller directory must exactly contain: ${requiredControllerNames.join(", ")}`,
    );
  }
  for (const scenarioID of gateRequirements.scenarioIds) {
    const controllerPath = secureExecutable(
      path.join(gateControllerDirectory, scenarioID),
      `${gate}/${scenarioID} scenario controller`,
    );
    scenarioControllers.set(`${gate}/${scenarioID}`, {
      path: controllerPath,
      sha256: sha256(readFileSync(controllerPath)),
    });
  }
}

const qualificationDirectory = path.join(outputDirectory, "qualification");
mkdirSync(qualificationDirectory, { mode: 0o755 });
const scenarioRoot = path.join(qualificationDirectory, "scenarios");
mkdirSync(scenarioRoot, { mode: 0o755 });
for (const gate of requestedGates) {
  runQualificationGate(gate);
}

function runQualificationGate(gate) {
  const gateRequirements = requirements.gates[gate];
  const gateStartedAt = new Date().toISOString();
  const gateScenarioDirectory = path.join(scenarioRoot, gate);
  mkdirSync(gateScenarioDirectory, { mode: 0o755 });
  const scenarioRecords = [];
  for (const scenarioID of gateRequirements.scenarioIds) {
    scenarioRecords.push(runQualificationScenario(gate, scenarioID));
  }
  const gateCompletedAt = new Date().toISOString();
  const recordSubjects = [...subjects.values()].map((subject) => ({
    id: subject.id,
    kind: subject.kind,
    locator: subject.locator,
    sha256: subject.digest.sha256,
  }));
  const record = {
    schemaVersion: 1,
    gate,
    releaseVersion: subjectManifest.releaseVersion,
    sourceCommit: subjectManifest.sourceCommit,
    subjectManifestSHA256,
    startedAt: gateStartedAt,
    completedAt: gateCompletedAt,
    status: "passed",
    summary: `Every required ${gate} scenario executed successfully against the packaged candidate`,
    hosts: hostInventory.hosts,
    subjects: recordSubjects,
    scenarios: scenarioRecords,
  };
  const recordPath = path.join(qualificationDirectory, `${gate}.json`);
  writeFileSync(recordPath, `${JSON.stringify(record, null, 2)}\n`, {
    encoding: "utf8",
    flag: "wx",
    mode: 0o644,
  });
  const verification = spawnSync(
    process.execPath,
    [
      path.join(repositoryRoot, "scripts", "verify-release-qualification-record.mjs"),
      subjectManifestPath,
      recordPath,
      gate,
      outputDirectory,
    ],
    { encoding: "utf8" },
  );
  if (verification.status !== 0) {
    fail(
      `${gate} record failed canonical verification: ${childFailureDetail(verification)}`,
    );
  }
  process.stdout.write(verification.stdout);
}

function runQualificationScenario(gate, scenarioID) {
  const scenarioDirectory = path.join(scenarioRoot, gate, scenarioID);
  mkdirSync(scenarioDirectory, { mode: 0o755 });
  const stdoutPath = path.join(scenarioDirectory, "controller.stdout.log");
  const stderrPath = path.join(scenarioDirectory, "controller.stderr.log");
  const stdoutFD = openSync(stdoutPath, "wx", 0o644);
  const stderrFD = openSync(stderrPath, "wx", 0o644);
  const controller = scenarioControllers.get(`${gate}/${scenarioID}`);
  const currentControllerPath = secureExecutable(
    controller.path,
    `${gate}/${scenarioID} scenario controller`,
  );
  if (
    currentControllerPath !== controller.path ||
    sha256(readFileSync(currentControllerPath)) !== controller.sha256
  ) {
    fail(`${gate}/${scenarioID} scenario controller changed after preflight`);
  }
  let execution;
  try {
    execution = spawnSync(
      currentControllerPath,
      [
        candidateDirectory,
        subjectManifestPath,
        hostInventoryPath,
        scenarioDirectory,
        gate,
        scenarioID,
      ],
      {
        cwd: candidateDirectory,
        env: process.env,
        stdio: ["ignore", stdoutFD, stderrFD],
      },
    );
  } finally {
    closeSync(stdoutFD);
    closeSync(stderrFD);
  }
  if (execution.error !== undefined || execution.status !== 0) {
    fail(
      `scenario ${gate}/${scenarioID} controller failed: ${childFailureDetail(execution)}`,
    );
  }
  verifyCandidateAuthority();

  const resultPath = secureScenarioFile(
    path.join(scenarioDirectory, "result.json"),
    scenarioDirectory,
    `${gate}/${scenarioID} result`,
  );
  const result = decodeJSON(
    readFileSync(resultPath),
    `${gate}/${scenarioID} result`,
  );
  validateDocument(
    compileRepositorySchema(
      "release/qualification-scenario-result-schema.json",
    ),
    result,
    `${gate}/${scenarioID} result`,
  );
  if (
    result.gate !== gate ||
    result.scenarioId !== scenarioID ||
    result.releaseVersion !== subjectManifest.releaseVersion ||
    result.sourceCommit !== subjectManifest.sourceCommit ||
    result.subjectManifestSHA256 !== subjectManifestSHA256
  ) {
    fail(`${gate}/${scenarioID} result candidate or scenario identity mismatch`);
  }
  for (const hostID of result.hostIds) {
    if (!hosts.has(hostID)) {
      fail(`${gate}/${scenarioID} result names unknown host ${hostID}`);
    }
  }
  if (
    !result.hostIds.some((hostID) =>
      isQualifiedRunnerHost(hosts.get(hostID)),
    )
  ) {
    fail(`${gate}/${scenarioID} did not execute on a qualified KVM runner host`);
  }
  requireKVMDeploymentHost(gate, scenarioID, result.hostIds);
  for (const subjectID of result.subjectIds) {
    if (!subjects.has(subjectID)) {
      fail(`${gate}/${scenarioID} result names unknown subject ${subjectID}`);
    }
  }

  const referencedArtifacts = new Set();
  for (const artifact of result.artifacts) {
    const normalizedPath = path.posix.normalize(artifact.path);
    if (
      path.posix.isAbsolute(artifact.path) ||
      normalizedPath !== artifact.path ||
      normalizedPath.startsWith("../") ||
      normalizedPath.includes("/../") ||
      artifact.path === "result.json" ||
      artifact.path === "controller.stdout.log" ||
      artifact.path === "controller.stderr.log" ||
      referencedArtifacts.has(artifact.path)
    ) {
      fail(
        `${gate}/${scenarioID} result has unsafe, reserved, or duplicate artifact ${artifact.path}`,
      );
    }
    const artifactPath = secureScenarioFile(
      path.join(scenarioDirectory, ...artifact.path.split("/")),
      scenarioDirectory,
      `${gate}/${scenarioID} artifact ${artifact.path}`,
    );
    if (statSync(artifactPath).size < 1) {
      fail(`${gate}/${scenarioID} artifact is empty: ${artifact.path}`);
    }
    if (sha256(readFileSync(artifactPath)) !== artifact.sha256) {
      fail(`${gate}/${scenarioID} artifact digest mismatch for ${artifact.path}`);
    }
    referencedArtifacts.add(artifact.path);
  }
  const actualFiles = walkRegularFiles(scenarioDirectory);
  const expectedFiles = [
    "controller.stderr.log",
    "controller.stdout.log",
    "result.json",
    ...referencedArtifacts,
  ].sort();
  if (
    actualFiles.length !== expectedFiles.length ||
    actualFiles.some((file, index) => file !== expectedFiles[index])
  ) {
    fail(
      `${gate}/${scenarioID} output contains missing, unexpected, or non-regular artifacts`,
    );
  }

  return {
    id: scenarioID,
    status: "passed",
    summary: result.summary,
    artifacts: actualFiles.map((relativePath) => ({
      path: path.posix.join(
        "qualification",
        "scenarios",
        gate,
        scenarioID,
        relativePath,
      ),
      sha256: sha256(
        readFileSync(
          path.join(scenarioDirectory, ...relativePath.split("/")),
        ),
      ),
    })),
  };
}

function requireKVMDeploymentHost(gate, scenarioID, hostIDs) {
  if (gate !== "kvm") {
    return;
  }
  let requiredMode;
  if (scenarioID === "packaged-systemd-runner") {
    requiredMode = "systemd";
  } else if (scenarioID === "packaged-compose-runner") {
    requiredMode = "compose";
  } else {
    return;
  }
  if (
    !hostIDs.some((hostID) => {
      const host = hosts.get(hostID);
      return isQualifiedRunnerHost(host) && host.deploymentMode === requiredMode;
    })
  ) {
    fail(
      `${gate}/${scenarioID} did not execute on the required ${requiredMode} KVM runner`,
    );
  }
}

function verifyCandidateSubject(subject) {
  if (subject.status !== "passed") {
    fail(`release subject ${subject.id} status is ${subject.status}`);
  }
  if (subject.kind === "oci-image") {
    if (
      !subject.locator.endsWith(`@sha256:${subject.digest.sha256}`)
    ) {
      fail(`release subject ${subject.id} OCI locator digest mismatch`);
    }
    return;
  }
  const normalizedLocator = path.posix.normalize(subject.locator);
  if (
    path.posix.isAbsolute(subject.locator) ||
    normalizedLocator !== subject.locator ||
    normalizedLocator.startsWith("../") ||
    normalizedLocator.includes("/../")
  ) {
    fail(`release subject ${subject.id} has unsafe candidate locator`);
  }
  const subjectPath = secureCandidateFile(
    path.join(candidateDirectory, ...subject.locator.split("/")),
    `release subject ${subject.id}`,
  );
  if (
    !Number.isSafeInteger(subject.sizeBytes) ||
    subject.sizeBytes < 1 ||
    statSync(subjectPath).size !== subject.sizeBytes
  ) {
    fail(`release subject ${subject.id} candidate size mismatch`);
  }
  if (sha256(readFileSync(subjectPath)) !== subject.digest.sha256) {
    fail(`release subject ${subject.id} candidate digest mismatch`);
  }
}

function verifyCandidateAuthority() {
  if (
    sha256(readFileSync(subjectManifestPath)) !== subjectManifestSHA256 ||
    sha256(readFileSync(hostInventoryPath)) !== hostInventorySHA256
  ) {
    fail("candidate manifest or qualified host inventory changed during qualification");
  }
  for (const subject of subjects.values()) {
    verifyCandidateSubject(subject);
  }
}

function isQualifiedRunnerHost(host) {
  return (
    host !== undefined &&
    host.role === "runner" &&
    host.operatingSystem === "linux" &&
    host.architecture === "amd64" &&
    host.dedicated === true &&
    host.kvm === true
  );
}

function secureDirectory(directoryPath, label) {
  const resolvedPath = path.resolve(directoryPath);
  let status;
  let canonicalPath;
  try {
    status = lstatSync(resolvedPath);
    canonicalPath = realpathSync(resolvedPath);
  } catch (error) {
    fail(`${label} is unavailable: ${error.message}`);
  }
  if (
    !status.isDirectory() ||
    status.isSymbolicLink() ||
    canonicalPath !== resolvedPath
  ) {
    fail(`${label} must be a canonical non-symbolic-link directory`);
  }
  return canonicalPath;
}

function secureFile(filePath, label) {
  const resolvedPath = path.resolve(filePath);
  let status;
  let canonicalPath;
  try {
    status = lstatSync(resolvedPath);
    canonicalPath = realpathSync(resolvedPath);
  } catch (error) {
    fail(`${label} is unavailable: ${error.message}`);
  }
  if (
    !status.isFile() ||
    status.isSymbolicLink() ||
    canonicalPath !== resolvedPath
  ) {
    fail(`${label} must be a canonical regular non-symbolic-link file`);
  }
  return canonicalPath;
}

function secureExecutable(filePath, label) {
  const canonicalPath = secureFile(filePath, label);
  if ((statSync(canonicalPath).mode & 0o111) === 0) {
    fail(`${label} must be executable`);
  }
  return canonicalPath;
}

function secureCandidateFile(filePath, label) {
  const canonicalPath = secureFile(filePath, label);
  if (!canonicalPath.startsWith(`${candidateDirectory}${path.sep}`)) {
    fail(`${label} escaped the candidate directory`);
  }
  return canonicalPath;
}

function secureScenarioFile(filePath, scenarioDirectory, label) {
  const canonicalPath = secureFile(filePath, label);
  if (!canonicalPath.startsWith(`${scenarioDirectory}${path.sep}`)) {
    fail(`${label} escaped its scenario artifact directory`);
  }
  return canonicalPath;
}

function walkRegularFiles(rootPath) {
  const files = [];
  const pending = [rootPath];
  while (pending.length > 0) {
    const directoryPath = pending.pop();
    for (const entry of readdirSync(directoryPath, { withFileTypes: true })) {
      const entryPath = path.join(directoryPath, entry.name);
      if (entry.isSymbolicLink()) {
        fail(
          `scenario artifact contains symbolic link: ${path.relative(rootPath, entryPath)}`,
        );
      }
      if (entry.isDirectory()) {
        if (realpathSync(entryPath) !== entryPath) {
          fail(
            `scenario artifact directory is not canonical: ${path.relative(rootPath, entryPath)}`,
          );
        }
        pending.push(entryPath);
      } else if (entry.isFile()) {
        files.push(
          path.relative(rootPath, entryPath).split(path.sep).join("/"),
        );
      } else {
        fail(
          `scenario artifact is not a regular file: ${path.relative(rootPath, entryPath)}`,
        );
      }
    }
  }
  return files.sort();
}

function compileRepositorySchema(relativePath) {
  const schema = readRepositoryJSON(relativePath, `${relativePath} schema`);
  return new Ajv2020({ allErrors: true, strict: true }).compile(schema);
}

function validateDocument(validate, document, label) {
  if (validate(document)) {
    return;
  }
  const details = validate.errors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
  fail(`${label} schema validation failed: ${details}`);
}

function readRepositoryJSON(relativePath, label) {
  return decodeJSON(
    readFileSync(path.join(repositoryRoot, relativePath)),
    label,
  );
}

function decodeJSON(contents, label) {
  try {
    return JSON.parse(contents);
  } catch (error) {
    fail(`${label} is not valid JSON: ${error.message}`);
  }
}

function indexUnique(entries, idKey, label) {
  const indexed = new Map();
  for (const entry of entries) {
    const entryID = entry[idKey];
    if (indexed.has(entryID)) {
      fail(`${label} has duplicate ${idKey} ${entryID}`);
    }
    indexed.set(entryID, entry);
  }
  return indexed;
}

function childFailureDetail(result) {
  if (result.error !== undefined) {
    return result.error.message;
  }
  const stderr = typeof result.stderr === "string" ? result.stderr.trim() : "";
  return `exit ${String(result.status)}${stderr === "" ? "" : `: ${stderr}`}`;
}

function sha256(contents) {
  return createHash("sha256").update(contents).digest("hex");
}

function fail(message) {
  console.error(`SecondBox packaged release qualification failed: ${message}`);
  process.exit(1);
}

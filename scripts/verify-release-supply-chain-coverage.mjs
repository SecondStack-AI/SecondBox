#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";

const requiredSubjectIDs = [
  "linux-release-package",
  "secondbox",
  "secondbox-artifact-evidence",
  "secondbox-guest-agent",
  "secondbox-runner",
  "secondbox-runner-identity",
  "secondboxd",
  "control-plane-image",
  "runner-image",
  "guest-execution-bundle",
  "guest-artifact-image",
  "go-sdk-package",
  "typescript-sdk-package",
];
const requiredSubjectKinds = new Map([
  ["linux-release-package", "release-package"],
  ["secondbox", "linux-binary"],
  ["secondbox-artifact-evidence", "linux-binary"],
  ["secondbox-guest-agent", "linux-binary"],
  ["secondbox-runner", "linux-binary"],
  ["secondbox-runner-identity", "linux-binary"],
  ["secondboxd", "linux-binary"],
  ["control-plane-image", "oci-image"],
  ["runner-image", "oci-image"],
  ["guest-execution-bundle", "guest-bundle"],
  ["guest-artifact-image", "oci-image"],
  ["go-sdk-package", "go-sdk"],
  ["typescript-sdk-package", "npm-package"],
]);

if (process.argv.length !== 5) {
  console.error(
    "Usage: scripts/verify-release-supply-chain-coverage.mjs SUBJECTS.json EVIDENCE.json EVIDENCE_TYPE",
  );
  process.exit(2);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(scriptDirectory, "..");
const subjectManifestPath = path.resolve(process.argv[2]);
const evidenceReportPath = path.resolve(process.argv[3]);
const expectedEvidenceType = process.argv[4];
const evidenceDirectory = path.dirname(subjectManifestPath);

function readJSON(documentPath, label) {
  try {
    return JSON.parse(fs.readFileSync(documentPath, "utf8"));
  } catch (error) {
    console.error(`SecondBox ${label} could not be decoded: ${error.message}`);
    process.exit(1);
  }
}

function compileSchema(schemaName) {
  const schemaPath = path.join(repositoryRoot, "release", schemaName);
  const schema = readJSON(schemaPath, `${schemaName} schema`);
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  return ajv.compile(schema);
}

function validateDocument(validate, document, label) {
  if (validate(document)) {
    return;
  }
  const details = validate.errors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
  console.error(`SecondBox ${label} schema validation failed: ${details}`);
  process.exit(1);
}

function indexUniqueSubjects(subjects, idKey, label) {
  const indexed = new Map();
  for (const subject of subjects) {
    const subjectID = subject[idKey];
    if (indexed.has(subjectID)) {
      console.error(`SecondBox ${label} has duplicate subject ${subjectID}`);
      process.exit(1);
    }
    indexed.set(subjectID, subject);
  }
  return indexed;
}

const subjectManifest = readJSON(subjectManifestPath, "release subject manifest");
const evidenceReport = readJSON(evidenceReportPath, "supply-chain evidence report");
validateDocument(
  compileSchema("supply-chain-subjects-schema.json"),
  subjectManifest,
  "release subject manifest",
);
validateDocument(
  compileSchema("supply-chain-evidence-schema.json"),
  evidenceReport,
  "supply-chain evidence",
);

if (evidenceReport.evidenceType !== expectedEvidenceType) {
  console.error(
    `SecondBox supply-chain evidence type ${evidenceReport.evidenceType} does not match ${expectedEvidenceType}`,
  );
  process.exit(1);
}
if (
  evidenceReport.releaseVersion !== subjectManifest.releaseVersion ||
  evidenceReport.sourceCommit !== subjectManifest.sourceCommit
) {
  console.error("SecondBox supply-chain evidence candidate identity mismatch");
  process.exit(1);
}

const manifestSubjects = indexUniqueSubjects(
  subjectManifest.subjects,
  "id",
  "release subject manifest",
);
const evidenceSubjects = indexUniqueSubjects(
  evidenceReport.subjects,
  "subjectId",
  "supply-chain evidence",
);
const guestBundleSubject = manifestSubjects.get("guest-execution-bundle");
const guestArtifactImageSubject = manifestSubjects.get("guest-artifact-image");
if (guestArtifactImageSubject?.status === "passed") {
  const binding = guestArtifactImageSubject.bindings?.[0];
  if (
    !/^ghcr\.io\/[a-z0-9._-]+(?:\/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}$/.test(
      guestArtifactImageSubject.locator,
    )
  ) {
    console.error(
      "SecondBox guest artifact image is not a digest-pinned GHCR subject",
    );
    process.exit(1);
  }
  if (
    guestBundleSubject?.status !== "passed" ||
    binding?.subjectId !== "guest-execution-bundle" ||
    binding?.digest?.sha256 !== guestBundleSubject.digest?.sha256
  ) {
    console.error(
      "SecondBox guest artifact image does not bind the exact signed guest bundle digest",
    );
    process.exit(1);
  }
}

for (const subjectID of requiredSubjectIDs) {
  if (!manifestSubjects.has(subjectID)) {
    console.error(`SecondBox release subject manifest is missing subject ${subjectID}`);
    process.exit(1);
  }
  if (!evidenceSubjects.has(subjectID)) {
    console.error(`SecondBox supply-chain evidence is missing subject ${subjectID}`);
    process.exit(1);
  }
}
for (const subjectID of manifestSubjects.keys()) {
  if (!requiredSubjectIDs.includes(subjectID)) {
    console.error(`SecondBox release subject manifest has unknown subject ${subjectID}`);
    process.exit(1);
  }
}
for (const subjectID of evidenceSubjects.keys()) {
  if (!requiredSubjectIDs.includes(subjectID)) {
    console.error(`SecondBox supply-chain evidence has unknown subject ${subjectID}`);
    process.exit(1);
  }
}

let everySubjectPassed = subjectManifest.status === "passed";
for (const subjectID of requiredSubjectIDs) {
  const subject = manifestSubjects.get(subjectID);
  const evidence = evidenceSubjects.get(subjectID);
  if (subject.kind !== requiredSubjectKinds.get(subjectID)) {
    console.error(
      `SecondBox release subject ${subjectID} has kind ${subject.kind}, expected ${requiredSubjectKinds.get(subjectID)}`,
    );
    process.exit(1);
  }
  const subjectSHA256 = subject.digest?.sha256;
  if (subject.status !== "passed") {
    everySubjectPassed = false;
    continue;
  }
  if (evidence.subjectSHA256 !== subjectSHA256) {
    console.error(
      `SecondBox supply-chain evidence digest mismatch for subject ${subjectID}`,
    );
    process.exit(1);
  }
  if (evidence.status !== "passed" || evidence.artifacts.length === 0) {
    everySubjectPassed = false;
  }
  for (const artifact of evidence.artifacts) {
    const normalizedPath = path.posix.normalize(artifact.path);
    if (
      path.isAbsolute(artifact.path) ||
      normalizedPath !== artifact.path ||
      normalizedPath.startsWith("../") ||
      normalizedPath.includes("/../")
    ) {
      console.error(
        `SecondBox supply-chain evidence has unsafe artifact path ${artifact.path}`,
      );
      process.exit(1);
    }
    const artifactPath = path.join(evidenceDirectory, ...artifact.path.split("/"));
    let artifactRealPath;
    try {
      const artifactLstat = fs.lstatSync(artifactPath);
      if (!artifactLstat.isFile() || artifactLstat.isSymbolicLink()) {
        throw new Error("not a regular non-symbolic-link file");
      }
      artifactRealPath = fs.realpathSync(artifactPath);
    } catch (error) {
      console.error(
        `SecondBox supply-chain evidence artifact ${artifact.path} is unavailable: ${error.message}`,
      );
      process.exit(1);
    }
    if (
      artifactRealPath !== artifactPath ||
      !artifactRealPath.startsWith(`${evidenceDirectory}${path.sep}`)
    ) {
      console.error(
        `SecondBox supply-chain evidence artifact ${artifact.path} escapes the evidence directory`,
      );
      process.exit(1);
    }
    const actualSHA256 = createHash("sha256")
      .update(fs.readFileSync(artifactRealPath))
      .digest("hex");
    if (actualSHA256 !== artifact.sha256) {
      console.error(
        `SecondBox supply-chain evidence artifact digest mismatch for ${artifact.path}`,
      );
      process.exit(1);
    }
  }
}

if (evidenceReport.status === "passed" && !everySubjectPassed) {
  console.error(
    "SecondBox supply-chain evidence cannot pass while a subject or its artifact evidence is incomplete",
  );
  process.exit(1);
}
if (everySubjectPassed && evidenceReport.status !== "passed") {
  console.error(
    "SecondBox supply-chain evidence status must pass when every exact subject is covered",
  );
  process.exit(1);
}

console.log(
  `SecondBox ${expectedEvidenceType} evidence accounts for every required release subject without digest drift`,
);

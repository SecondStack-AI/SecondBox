#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  lstatSync,
  readFileSync,
  realpathSync,
  statSync,
} from "node:fs";
import path from "node:path";

if (process.argv.length !== 4) {
  console.error(
    "Usage: scripts/verify-qualified-publication-inputs.mjs WORKFLOW_RUN_EVENT.json RELEASE_EVIDENCE.json",
  );
  process.exit(2);
}

const expectedRepository = "SecondStack-AI/SecondBox";
const expectedEvidenceWorkflow = ".github/workflows/release-evidence.yml";
const expectedBranch = "main";
const requiredSubjectDefinitions = new Map([
  [
    "linux-release-package",
    {
      kind: "release-package",
      locator: (version) =>
        `package/secondbox-${version}-linux-amd64.tar.gz`,
    },
  ],
  [
    "secondbox",
    { kind: "linux-binary", locator: () => "dist/secondbox" },
  ],
  [
    "secondbox-artifact-evidence",
    {
      kind: "linux-binary",
      locator: () => "dist/secondbox-artifact-evidence",
    },
  ],
  [
    "secondbox-guest-agent",
    {
      kind: "linux-binary",
      locator: () => "dist/secondbox-guest-agent",
    },
  ],
  [
    "secondbox-runner",
    { kind: "linux-binary", locator: () => "dist/secondbox-runner" },
  ],
  [
    "secondbox-runner-identity",
    {
      kind: "linux-binary",
      locator: () => "dist/secondbox-runner-identity",
    },
  ],
  [
    "secondboxd",
    { kind: "linux-binary", locator: () => "dist/secondboxd" },
  ],
  [
    "control-plane-image",
    {
      kind: "oci-image",
      repository: "ghcr.io/secondstack-ai/secondbox-control-plane",
    },
  ],
  [
    "runner-image",
    {
      kind: "oci-image",
      repository: "ghcr.io/secondstack-ai/secondbox-runner",
    },
  ],
  [
    "guest-execution-bundle",
    {
      kind: "guest-bundle",
      locator: (version) =>
        `guest/secondbox-${version}-guest-amd64.tar.gz`,
    },
  ],
  [
    "guest-artifact-image",
    {
      kind: "oci-image",
      repository: "ghcr.io/secondstack-ai/secondbox-guest-artifacts",
    },
  ],
  [
    "go-sdk-package",
    {
      kind: "go-sdk",
      locator: (version) => `sdk/secondbox-${version}-go-sdk.tar.gz`,
    },
  ],
  [
    "typescript-sdk-package",
    {
      kind: "npm-package",
      locator: (version) =>
        `sdk/secondstack-ai-secondbox-${version}.tgz`,
    },
  ],
]);

const eventPath = path.resolve(process.argv[2]);
const evidencePath = path.resolve(process.argv[3]);
const event = readPublicationJSON(eventPath, "workflow_run event");
const evidence = readPublicationJSON(evidencePath, "release evidence");
const evidenceDirectory = realpathSync(path.dirname(evidencePath));

verifyWorkflowRunEvent(event);
verifyEvidenceIdentity(event, evidence);

const subjectManifestPath = resolveEvidenceFile(
  evidenceDirectory,
  evidence.subjects,
  "release subject manifest",
);
const subjectManifest = readPublicationJSON(
  subjectManifestPath,
  "release subject manifest",
);
verifyReleaseSubjects(evidence, subjectManifest, evidenceDirectory);

const publicationIdentity = {
  repository: expectedRepository,
  releaseVersion: evidence.releaseVersion,
  releaseTag: `v${evidence.releaseVersion}`,
  sourceCommit: evidence.sourceCommit,
  evidenceWorkflowRunId: event.workflow_run.id,
  evidenceArtifactName: `release-evidence-${evidence.sourceCommit}`,
  subjects: Object.fromEntries(
    subjectManifest.subjects.map((subject) => [
      subject.id,
      {
        locator: subject.locator,
        sha256: subject.digest.sha256,
      },
    ]),
  ),
};
process.stdout.write(`${JSON.stringify(publicationIdentity, null, 2)}\n`);

function failPublicationInput(message) {
  console.error(`SecondBox qualified publication input invalid: ${message}`);
  process.exit(1);
}

function readPublicationJSON(documentPath, label) {
  try {
    return JSON.parse(readFileSync(documentPath, "utf8"));
  } catch (error) {
    failPublicationInput(`${label} could not be decoded: ${error.message}`);
  }
}

function verifyWorkflowRunEvent(eventDocument) {
  if (
    eventDocument.action !== "completed" ||
    eventDocument.workflow_run?.status !== "completed" ||
    eventDocument.workflow_run?.conclusion !== "success"
  ) {
    failPublicationInput(
      "publication requires a successfully completed evidence workflow_run",
    );
  }
  if (
    eventDocument.repository?.full_name !== expectedRepository ||
    eventDocument.workflow_run?.head_repository?.full_name !==
      expectedRepository
  ) {
    failPublicationInput(
      `workflow_run repository must be ${expectedRepository}`,
    );
  }
  if (
    eventDocument.repository?.private !== false ||
    (eventDocument.repository?.visibility !== undefined &&
      eventDocument.repository.visibility !== "public")
  ) {
    failPublicationInput(
      "the SecondBox repository must be public before publication",
    );
  }
  if (
    eventDocument.workflow_run?.path !== expectedEvidenceWorkflow ||
    eventDocument.workflow_run?.event !== "workflow_dispatch" ||
    eventDocument.workflow_run?.head_branch !== expectedBranch
  ) {
    failPublicationInput(
      `publication requires manually dispatched ${expectedEvidenceWorkflow} from protected ${expectedBranch}`,
    );
  }
  if (
    !Number.isSafeInteger(eventDocument.workflow_run?.id) ||
    eventDocument.workflow_run.id < 1
  ) {
    failPublicationInput("workflow_run id must be a positive integer");
  }
  if (
    !/^[0-9a-f]{40}$/.test(eventDocument.workflow_run?.head_sha ?? "")
  ) {
    failPublicationInput(
      "workflow_run head_sha must be a full lowercase commit SHA",
    );
  }
}

function verifyEvidenceIdentity(eventDocument, evidenceDocument) {
  if (
    !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(
      evidenceDocument.releaseVersion ?? "",
    )
  ) {
    failPublicationInput(
      "release evidence version must be SemVer without a leading v or build metadata",
    );
  }
  if (
    evidenceDocument.sourceCommit !== eventDocument.workflow_run.head_sha
  ) {
    failPublicationInput(
      "release evidence source commit does not match workflow_run head_sha",
    );
  }
}

function resolveEvidenceFile(evidenceRoot, relativePath, label) {
  if (
    typeof relativePath !== "string" ||
    relativePath.length === 0 ||
    path.posix.isAbsolute(relativePath) ||
    path.posix.normalize(relativePath) !== relativePath ||
    relativePath.startsWith("../") ||
    relativePath.includes("/../")
  ) {
    failPublicationInput(`${label} has an unsafe path`);
  }
  const candidatePath = path.join(evidenceRoot, ...relativePath.split("/"));
  let fileStatus;
  let resolvedPath;
  try {
    fileStatus = lstatSync(candidatePath);
    resolvedPath = realpathSync(candidatePath);
  } catch (error) {
    failPublicationInput(`${label} is unavailable: ${error.message}`);
  }
  if (
    !fileStatus.isFile() ||
    fileStatus.isSymbolicLink() ||
    resolvedPath !== candidatePath ||
    !resolvedPath.startsWith(`${evidenceRoot}${path.sep}`)
  ) {
    failPublicationInput(
      `${label} must be a regular file inside the evidence directory`,
    );
  }
  return resolvedPath;
}

function verifyReleaseSubjects(
  evidenceDocument,
  subjectManifest,
  evidenceRoot,
) {
  if (
    subjectManifest.schemaVersion !== 1 ||
    subjectManifest.status !== "passed" ||
    subjectManifest.releaseVersion !== evidenceDocument.releaseVersion ||
    subjectManifest.sourceCommit !== evidenceDocument.sourceCommit ||
    !Array.isArray(subjectManifest.subjects) ||
    subjectManifest.subjects.length !== requiredSubjectDefinitions.size
  ) {
    failPublicationInput(
      "release subject manifest identity, status, or cardinality is invalid",
    );
  }

  const indexedSubjects = new Map();
  for (const subject of subjectManifest.subjects) {
    if (
      typeof subject?.id !== "string" ||
      indexedSubjects.has(subject.id)
    ) {
      failPublicationInput(
        `release subject manifest has duplicate or invalid id ${String(subject?.id)}`,
      );
    }
    indexedSubjects.set(subject.id, subject);
  }

  for (const [subjectID, definition] of requiredSubjectDefinitions) {
    const subject = indexedSubjects.get(subjectID);
    if (subject === undefined) {
      failPublicationInput(`release subject manifest is missing ${subjectID}`);
    }
    if (
      subject.kind !== definition.kind ||
      subject.status !== "passed" ||
      !/^[0-9a-f]{64}$/.test(subject.digest?.sha256 ?? "")
    ) {
      failPublicationInput(
        `release subject ${subjectID} kind, status, or digest is invalid`,
      );
    }

    if (definition.kind === "oci-image") {
      const expectedLocator = `${definition.repository}@sha256:${subject.digest.sha256}`;
      if (subject.locator !== expectedLocator) {
        failPublicationInput(
          `release subject ${subjectID} must use exact locator ${expectedLocator}`,
        );
      }
      continue;
    }

    const expectedLocator = definition.locator(
      evidenceDocument.releaseVersion,
    );
    if (subject.locator !== expectedLocator) {
      failPublicationInput(
        `release subject ${subjectID} locator must be ${expectedLocator}`,
      );
    }
    const subjectPath = resolveEvidenceFile(
      evidenceRoot,
      subject.locator,
      `release subject ${subjectID}`,
    );
    const contents = readFileSync(subjectPath);
    const actualDigest = createHash("sha256").update(contents).digest("hex");
    if (
      actualDigest !== subject.digest.sha256 ||
      statSync(subjectPath).size !== subject.sizeBytes
    ) {
      failPublicationInput(
        `release subject ${subjectID} bytes do not match its signed record`,
      );
    }
  }

  for (const subjectID of indexedSubjects.keys()) {
    if (!requiredSubjectDefinitions.has(subjectID)) {
      failPublicationInput(
        `release subject manifest contains unknown subject ${subjectID}`,
      );
    }
  }

  const guestBundle = indexedSubjects.get("guest-execution-bundle");
  const guestArtifactImage = indexedSubjects.get("guest-artifact-image");
  if (
    guestArtifactImage.bindings?.length !== 1 ||
    guestArtifactImage.bindings[0]?.subjectId !==
      "guest-execution-bundle" ||
    guestArtifactImage.bindings[0]?.digest?.sha256 !==
      guestBundle.digest.sha256
  ) {
    failPublicationInput(
      "guest artifact image does not bind the exact signed guest bundle",
    );
  }
}

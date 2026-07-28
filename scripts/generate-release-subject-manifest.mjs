#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  lstatSync,
  readFileSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";

if (process.argv.length !== 4) {
  console.error(
    "Usage: scripts/generate-release-subject-manifest.mjs EVIDENCE_DIRECTORY OUTPUT.json",
  );
  process.exit(2);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(scriptDirectory, "..");
const evidenceDirectory = realpathSync(process.argv[2]);
const outputPath = path.resolve(process.argv[3]);
const releaseVersion = requireEnvironment("SECONDBOX_RELEASE_VERSION");
const sourceCommit = requireEnvironment("SECONDBOX_RELEASE_SOURCE_COMMIT");

if (!/^[0-9a-f]{40}$/.test(sourceCommit)) {
  fail("SECONDBOX_RELEASE_SOURCE_COMMIT must be 40 lowercase hexadecimal characters");
}
const repositoryCommit = spawnSync(
  "git",
  ["-C", repositoryRoot, "rev-parse", "HEAD"],
  { encoding: "utf8" },
);
if (repositoryCommit.status !== 0 || repositoryCommit.stdout.trim() !== sourceCommit) {
  fail("SecondBox release subject source commit must equal the checked-out commit");
}
if (path.dirname(outputPath) !== evidenceDirectory) {
  fail("SecondBox release subject manifest must be written at the evidence root");
}
try {
  lstatSync(outputPath);
  fail(`SecondBox release subject manifest refuses to overwrite: ${outputPath}`);
} catch (error) {
  if (error.code !== "ENOENT") {
    throw error;
  }
}

const localSubjectDefinitions = [
  {
    id: "linux-release-package",
    kind: "release-package",
    relativePath: `package/secondbox-${releaseVersion}-linux-amd64.tar.gz`,
  },
  { id: "secondbox", kind: "linux-binary", relativePath: "dist/secondbox" },
  {
    id: "secondbox-artifact-evidence",
    kind: "linux-binary",
    relativePath: "dist/secondbox-artifact-evidence",
  },
  {
    id: "secondbox-guest-agent",
    kind: "linux-binary",
    relativePath: "dist/secondbox-guest-agent",
  },
  {
    id: "secondbox-runner",
    kind: "linux-binary",
    relativePath: "dist/secondbox-runner",
  },
  {
    id: "secondbox-runner-identity",
    kind: "linux-binary",
    relativePath: "dist/secondbox-runner-identity",
  },
  { id: "secondboxd", kind: "linux-binary", relativePath: "dist/secondboxd" },
  {
    id: "guest-execution-bundle",
    kind: "guest-bundle",
    relativePath: `guest/secondbox-${releaseVersion}-guest-amd64.tar.gz`,
  },
  {
    id: "go-sdk-package",
    kind: "go-sdk",
    relativePath: `sdk/secondbox-${releaseVersion}-go-sdk.tar.gz`,
  },
  {
    id: "typescript-sdk-package",
    kind: "npm-package",
    relativePath: `sdk/secondstack-ai-secondbox-${releaseVersion}.tgz`,
  },
];

const subjects = localSubjectDefinitions.map(resolveLocalSubject);
subjects.splice(
  7,
  0,
  resolveOCIImageSubject(
    "control-plane-image",
    "SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE",
  ),
  resolveOCIImageSubject("runner-image", "SECONDBOX_RELEASE_RUNNER_IMAGE"),
);
const guestBundleSubject = subjects.find(
  (subject) => subject.id === "guest-execution-bundle",
);
subjects.splice(
  10,
  0,
  resolveBoundOCIImageSubject(
    "guest-artifact-image",
    "SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE",
    guestBundleSubject,
  ),
);

const allPassed = subjects.every((subject) => subject.status === "passed");
const manifest = {
  schemaVersion: 1,
  releaseVersion,
  sourceCommit,
  status: allPassed ? "passed" : "blocked",
  summary: allPassed
    ? "Every required release package, binary, image, guest bundle, and SDK package resolved to an immutable digest"
    : "One or more required release subjects are absent, invalid, or not registry-verified",
  subjects,
};

validateManifest(manifest);
writeFileSync(outputPath, `${JSON.stringify(manifest, null, 2)}\n`, {
  encoding: "utf8",
  flag: "wx",
  mode: 0o644,
});
console.log(outputPath);

function requireEnvironment(name) {
  const value = process.env[name];
  if (value === undefined || value.length === 0) {
    fail(`set ${name}`);
  }
  return value;
}

function fail(message) {
  console.error(message);
  process.exit(1);
}

function resolveLocalSubject(definition) {
  const candidatePath = path.join(evidenceDirectory, definition.relativePath);
  try {
    const fileLstat = lstatSync(candidatePath);
    if (!fileLstat.isFile() || fileLstat.isSymbolicLink()) {
      throw new Error("not a regular non-symbolic-link file");
    }
    const canonicalPath = realpathSync(candidatePath);
    if (
      canonicalPath !== candidatePath ||
      !canonicalPath.startsWith(`${evidenceDirectory}${path.sep}`)
    ) {
      throw new Error("escapes the evidence directory");
    }
    const contents = readFileSync(canonicalPath);
    return {
      id: definition.id,
      kind: definition.kind,
      status: "passed",
      summary: "Candidate file is present and content-addressed",
      locator: definition.relativePath,
      digest: { sha256: createHash("sha256").update(contents).digest("hex") },
      sizeBytes: statSync(canonicalPath).size,
    };
  } catch (error) {
    return {
      id: definition.id,
      kind: definition.kind,
      status: "blocked",
      summary: `Candidate file ${definition.relativePath} is unavailable: ${error.message}`,
    };
  }
}

function resolveOCIImageSubject(subjectID, environmentName) {
  const imageReference = process.env[environmentName];
  if (imageReference === undefined || imageReference.length === 0) {
    return {
      id: subjectID,
      kind: "oci-image",
      status: "blocked",
      summary: `${environmentName} is not set to a registry-backed digest reference`,
    };
  }
  const digestMatch = imageReference.match(/@sha256:([0-9a-f]{64})$/);
  if (digestMatch === null) {
    return {
      id: subjectID,
      kind: "oci-image",
      status: "blocked",
      summary: `${environmentName} is not pinned by a sha256 registry digest`,
    };
  }
  const inspection = spawnSync(
    "docker",
    ["buildx", "imagetools", "inspect", imageReference],
    { encoding: "utf8" },
  );
  if (inspection.status !== 0) {
    const detail = inspection.error?.message ?? inspection.stderr.trim();
    return {
      id: subjectID,
      kind: "oci-image",
      status: "blocked",
      summary: `${environmentName} could not be resolved from its registry: ${detail || "docker buildx inspect failed"}`,
    };
  }
  return {
    id: subjectID,
    kind: "oci-image",
    status: "passed",
    summary: "Digest-pinned OCI image resolved from its registry",
    locator: imageReference,
    digest: { sha256: digestMatch[1] },
  };
}

function resolveBoundOCIImageSubject(
  subjectID,
  environmentName,
  boundGuestBundle,
) {
  const imageReference = process.env[environmentName];
  if (
    imageReference !== undefined &&
    imageReference.length > 0 &&
    !/^ghcr\.io\/[a-z0-9._-]+(?:\/[a-z0-9._-]+)+@sha256:[0-9a-f]{64}$/.test(
      imageReference,
    )
  ) {
    return {
      id: subjectID,
      kind: "oci-image",
      status: "blocked",
      summary: `${environmentName} is not a digest-pinned GHCR reference`,
    };
  }
  const imageSubject = resolveOCIImageSubject(subjectID, environmentName);
  if (imageSubject.status !== "passed") {
    return imageSubject;
  }
  if (boundGuestBundle?.status !== "passed") {
    return {
      id: subjectID,
      kind: "oci-image",
      status: "blocked",
      summary:
        "The digest-pinned guest artifact image cannot bind an unavailable signed guest bundle",
    };
  }
  return {
    ...imageSubject,
    summary:
      "Digest-pinned GHCR transport image resolved and binds the signed guest bundle",
    bindings: [
      {
        subjectId: boundGuestBundle.id,
        digest: boundGuestBundle.digest,
      },
    ],
  };
}

function validateManifest(manifest) {
  const schema = JSON.parse(
    readFileSync(
      path.join(repositoryRoot, "release", "supply-chain-subjects-schema.json"),
      "utf8",
    ),
  );
  const validate = new Ajv2020({ allErrors: true, strict: true }).compile(schema);
  if (validate(manifest)) {
    return;
  }
  const details = validate.errors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
  fail(`SecondBox release subject manifest schema validation failed: ${details}`);
}

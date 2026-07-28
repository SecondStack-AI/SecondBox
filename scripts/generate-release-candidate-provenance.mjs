#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  lstatSync,
  mkdirSync,
  readFileSync,
  realpathSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";

if (process.argv.length !== 9) {
  console.error(
    "Usage: scripts/generate-release-candidate-provenance.mjs OUTPUT_DIRECTORY RELEASE_VERSION SOURCE_COMMIT GUEST_ARCHIVE CONTROL_PLANE_IMAGE RUNNER_IMAGE GUEST_ARTIFACT_IMAGE",
  );
  process.exit(2);
}

const outputDirectory = secureDirectory(process.argv[2], "provenance output");
const releaseVersion = process.argv[3];
const sourceCommit = process.argv[4];
const guestArchive = secureFile(process.argv[5], "signed guest bundle");
const builderIdentity = requireEnvironment(
  "SECONDBOX_RELEASE_CANDIDATE_BUILDER_IDENTITY",
);
const imageReferences = new Map([
  ["control-plane-image", process.argv[6]],
  ["runner-image", process.argv[7]],
  ["guest-artifact-image", process.argv[8]],
]);

if (
  !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(
    releaseVersion,
  )
) {
  fail("release version must be SemVer without a leading v or build metadata");
}
if (!/^[0-9a-f]{40}$/.test(sourceCommit)) {
  fail("source commit must be a full lowercase commit SHA");
}
for (const [subjectID, imageReference] of imageReferences) {
  if (
    !/^ghcr\.io\/secondstack-ai\/secondbox-(?:control-plane|runner|guest-artifacts)@sha256:[0-9a-f]{64}$/.test(
      imageReference,
    )
  ) {
    fail(`${subjectID} must use its canonical digest-pinned GHCR locator`);
  }
}

const externalProvenanceDirectory = path.join(
  outputDirectory,
  "external-provenance",
);
mkdirSync(externalProvenanceDirectory, { recursive: false, mode: 0o755 });
const guestDigest = sha256File(guestArchive);
writeProvenance(
  "guest-execution-bundle",
  path.basename(guestArchive),
  guestDigest,
  [],
);
for (const [subjectID, imageReference] of imageReferences) {
  const digest = imageReference.slice(imageReference.indexOf("@sha256:") + 8);
  const resolvedDependencies =
    subjectID === "guest-artifact-image"
      ? [
          {
            uri: `secondbox-release:guest/${path.basename(guestArchive)}`,
            digest: { sha256: guestDigest },
          },
        ]
      : [];
  writeProvenance(subjectID, imageReference, digest, resolvedDependencies);
}

function writeProvenance(
  subjectID,
  subjectName,
  subjectSHA256,
  resolvedDependencies,
) {
  const outputPath = path.join(
    externalProvenanceDirectory,
    `${subjectID}.intoto.json`,
  );
  const statement = {
    _type: "https://in-toto.io/Statement/v1",
    subject: [{ name: subjectName, digest: { sha256: subjectSHA256 } }],
    predicateType: "https://slsa.dev/provenance/v1",
    predicate: {
      buildDefinition: {
        buildType: "https://secondbox.dev/build-types/protected-release-candidate/v1",
        externalParameters: { releaseVersion, sourceCommit },
        internalParameters: {},
        resolvedDependencies: [
          {
            uri: `git+https://github.com/SecondStack-AI/SecondBox@${sourceCommit}`,
            digest: { gitCommit: sourceCommit },
          },
          ...resolvedDependencies,
        ],
      },
      runDetails: {
        builder: { id: builderIdentity },
        metadata: {
          invocationId: builderIdentity,
        },
      },
    },
  };
  writeFileSync(outputPath, `${JSON.stringify(statement, null, 2)}\n`, {
    encoding: "utf8",
    flag: "wx",
    mode: 0o644,
  });
}

function secureDirectory(directoryPath, label) {
  const resolvedPath = path.resolve(directoryPath);
  try {
    const status = lstatSync(resolvedPath);
    const canonicalPath = realpathSync(resolvedPath);
    if (
      !status.isDirectory() ||
      status.isSymbolicLink() ||
      canonicalPath !== resolvedPath
    ) {
      fail(`${label} must be a canonical non-symbolic-link directory`);
    }
    return canonicalPath;
  } catch (error) {
    fail(`${label} is unavailable: ${error.message}`);
  }
}

function secureFile(filePath, label) {
  const resolvedPath = path.resolve(filePath);
  try {
    const status = lstatSync(resolvedPath);
    const canonicalPath = realpathSync(resolvedPath);
    if (
      !status.isFile() ||
      status.isSymbolicLink() ||
      canonicalPath !== resolvedPath
    ) {
      fail(`${label} must be a canonical regular non-symbolic-link file`);
    }
    return canonicalPath;
  } catch (error) {
    fail(`${label} is unavailable: ${error.message}`);
  }
}

function sha256File(filePath) {
  return createHash("sha256").update(readFileSync(filePath)).digest("hex");
}

function requireEnvironment(name) {
  const value = process.env[name];
  if (value === undefined || value.length === 0) {
    fail(`set ${name}`);
  }
  return value;
}

function fail(message) {
  console.error(`SecondBox release candidate provenance failed: ${message}`);
  process.exit(1);
}

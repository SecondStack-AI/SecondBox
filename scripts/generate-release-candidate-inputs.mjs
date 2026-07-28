#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  lstatSync,
  readFileSync,
  readdirSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";

if (process.argv.length !== 9) {
  console.error(
    "Usage: scripts/generate-release-candidate-inputs.mjs CANDIDATE_DIRECTORY RELEASE_VERSION SOURCE_COMMIT CONTROL_PLANE_IMAGE RUNNER_IMAGE GUEST_ARTIFACT_IMAGE GUEST_ARCHIVE",
  );
  process.exit(2);
}

const candidateDirectory = secureDirectory(process.argv[2]);
const releaseVersion = process.argv[3];
const sourceCommit = process.argv[4];
const imageReferences = {
  controlPlaneImage: process.argv[5],
  runnerImage: process.argv[6],
  guestArtifactImage: process.argv[7],
};
const guestArchive = secureFileOutsideCandidate(
  process.argv[8],
  "signed guest archive",
);
const outputPath = path.join(candidateDirectory, "release-candidate-inputs.json");

if (
  !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(
    releaseVersion,
  ) ||
  !/^[0-9a-f]{40}$/.test(sourceCommit)
) {
  fail("candidate version or source commit is invalid");
}
const expectedImagePatterns = {
  controlPlaneImage:
    /^ghcr\.io\/secondstack-ai\/secondbox-control-plane@sha256:[0-9a-f]{64}$/,
  runnerImage:
    /^ghcr\.io\/secondstack-ai\/secondbox-runner@sha256:[0-9a-f]{64}$/,
  guestArtifactImage:
    /^ghcr\.io\/secondstack-ai\/secondbox-guest-artifacts@sha256:[0-9a-f]{64}$/,
};
for (const [name, imageReference] of Object.entries(imageReferences)) {
  if (!expectedImagePatterns[name].test(imageReference)) {
    fail(`${name} is not its canonical digest-pinned GHCR reference`);
  }
}

const expectedFiles = [
  "dist/SHA256SUMS",
  "dist/secondbox",
  "dist/secondbox-artifact-evidence",
  "dist/secondbox-guest-agent",
  "dist/secondbox-runner",
  "dist/secondbox-runner-identity",
  "dist/secondboxd",
  `package/secondbox-${releaseVersion}-linux-amd64.SHA256SUMS`,
  `package/secondbox-${releaseVersion}-linux-amd64.manifest.json`,
  `package/secondbox-${releaseVersion}-linux-amd64.tar.gz`,
  `sdk/secondbox-${releaseVersion}-go-sdk.tar.gz`,
  `sdk/secondbox-${releaseVersion}-sdk.SHA256SUMS`,
  `sdk/secondstack-ai-secondbox-${releaseVersion}.tgz`,
  "external-provenance/control-plane-image.intoto.json",
  "external-provenance/runner-image.intoto.json",
  "external-provenance/guest-execution-bundle.intoto.json",
  "external-provenance/guest-artifact-image.intoto.json",
].sort();
const actualFiles = walkCandidateFiles(candidateDirectory).filter(
  (relativePath) =>
    relativePath !== "protected-environment.json" &&
    relativePath !== "protected-workflow-identity.json" &&
    relativePath !== "release-candidate-inputs.json",
);
if (
  actualFiles.length !== expectedFiles.length ||
  expectedFiles.some((relativePath, index) => actualFiles[index] !== relativePath)
) {
  fail(
    `candidate artifact file set differs from the canonical allowlist: ${actualFiles.join(", ")}`,
  );
}

const files = expectedFiles.map((relativePath) => {
  const filePath = path.join(candidateDirectory, ...relativePath.split("/"));
  return {
    path: relativePath,
    sha256: createHash("sha256")
      .update(readFileSync(filePath))
      .digest("hex"),
    sizeBytes: statSync(filePath).size,
  };
});
const manifest = {
  schemaVersion: 1,
  releaseVersion,
  sourceCommit,
  images: imageReferences,
  guestBundle: {
    locator: `guest/secondbox-${releaseVersion}-guest-amd64.tar.gz`,
    sha256: createHash("sha256")
      .update(readFileSync(guestArchive))
      .digest("hex"),
    sizeBytes: statSync(guestArchive).size,
  },
  provenance: {
    controlPlaneImage:
      "external-provenance/control-plane-image.intoto.json",
    runnerImage: "external-provenance/runner-image.intoto.json",
    guestExecutionBundle:
      "external-provenance/guest-execution-bundle.intoto.json",
    guestArtifactImage:
      "external-provenance/guest-artifact-image.intoto.json",
  },
  files,
};
writeFileSync(outputPath, `${JSON.stringify(manifest, null, 2)}\n`, {
  encoding: "utf8",
  flag: "wx",
  mode: 0o644,
});

function walkCandidateFiles(rootPath) {
  const files = [];
  const pending = [rootPath];
  while (pending.length > 0) {
    const directoryPath = pending.pop();
    for (const entry of readdirSync(directoryPath, { withFileTypes: true })) {
      const entryPath = path.join(directoryPath, entry.name);
      if (entry.isSymbolicLink()) {
        fail(`candidate artifact contains symbolic link: ${entryPath}`);
      }
      if (entry.isDirectory()) {
        pending.push(entryPath);
      } else if (entry.isFile()) {
        files.push(path.relative(rootPath, entryPath).split(path.sep).join("/"));
      } else {
        fail(`candidate artifact contains non-regular file: ${entryPath}`);
      }
    }
  }
  return files.sort();
}

function secureDirectory(directoryPath) {
  const resolvedPath = path.resolve(directoryPath);
  const status = lstatSync(resolvedPath);
  const canonicalPath = realpathSync(resolvedPath);
  if (
    !status.isDirectory() ||
    status.isSymbolicLink() ||
    canonicalPath !== resolvedPath
  ) {
    fail("candidate directory must be canonical and non-symbolic");
  }
  return canonicalPath;
}

function secureFileOutsideCandidate(filePath, label) {
  const resolvedPath = path.resolve(filePath);
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
}

function fail(message) {
  console.error(`SecondBox release candidate input generation failed: ${message}`);
  process.exit(1);
}

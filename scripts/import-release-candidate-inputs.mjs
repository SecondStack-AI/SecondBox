#!/usr/bin/env node

import { createHash } from "node:crypto";
import {
  constants as fsConstants,
  copyFileSync,
  lstatSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  realpathSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";

if (process.argv.length !== 7) {
  console.error(
    "Usage: scripts/import-release-candidate-inputs.mjs SOURCE_DIRECTORY EVIDENCE_DIRECTORY RELEASE_VERSION SOURCE_COMMIT GITHUB_ENV",
  );
  process.exit(2);
}

const sourceDirectory = secureDirectory(process.argv[2], "candidate source");
const evidenceDirectory = secureDirectory(process.argv[3], "evidence destination");
const expectedVersion = process.argv[4];
const expectedCommit = process.argv[5];
const githubEnvironmentPath = path.resolve(process.argv[6]);
const manifestPath = secureFile(
  path.join(sourceDirectory, "release-candidate-inputs.json"),
  "candidate manifest",
);
const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));

if (
  !hasExactKeys(manifest, [
    "schemaVersion",
    "releaseVersion",
    "sourceCommit",
    "images",
    "guestBundle",
    "provenance",
    "files",
  ]) ||
  manifest.schemaVersion !== 1 ||
  manifest.releaseVersion !== expectedVersion ||
  manifest.sourceCommit !== expectedCommit ||
  !Array.isArray(manifest.files) ||
  manifest.files.length !== 17
) {
  fail("candidate manifest identity or canonical file count is invalid");
}
if (
  !hasExactKeys(manifest.images, [
    "controlPlaneImage",
    "runnerImage",
    "guestArtifactImage",
  ]) ||
  !hasExactKeys(manifest.guestBundle, [
    "locator",
    "sha256",
    "sizeBytes",
  ]) ||
  !hasExactKeys(manifest.provenance, [
    "controlPlaneImage",
    "runnerImage",
    "guestExecutionBundle",
    "guestArtifactImage",
  ])
) {
  fail("candidate manifest contains missing or unexpected identity fields");
}

const imageDefinitions = [
  [
    "controlPlaneImage",
    "SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE",
    /^ghcr\.io\/secondstack-ai\/secondbox-control-plane@sha256:[0-9a-f]{64}$/,
  ],
  [
    "runnerImage",
    "SECONDBOX_RELEASE_RUNNER_IMAGE",
    /^ghcr\.io\/secondstack-ai\/secondbox-runner@sha256:[0-9a-f]{64}$/,
  ],
  [
    "guestArtifactImage",
    "SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE",
    /^ghcr\.io\/secondstack-ai\/secondbox-guest-artifacts@sha256:[0-9a-f]{64}$/,
  ],
];
const environmentLines = [];
for (const [field, environmentName, pattern] of imageDefinitions) {
  const value = manifest.images?.[field];
  if (typeof value !== "string" || !pattern.test(value)) {
    fail(`${field} is not its canonical digest-pinned GHCR reference`);
  }
  environmentLines.push(`${environmentName}=${value}`);
}
const expectedGuestLocator = `guest/secondbox-${expectedVersion}-guest-amd64.tar.gz`;
if (
  manifest.guestBundle?.locator !== expectedGuestLocator ||
  !/^[0-9a-f]{64}$/.test(manifest.guestBundle?.sha256 ?? "") ||
  !Number.isSafeInteger(manifest.guestBundle?.sizeBytes) ||
  manifest.guestBundle.sizeBytes < 1
) {
  fail("candidate manifest signed guest bundle identity is invalid");
}
environmentLines.push(
  `SECONDBOX_RELEASE_EXPECTED_GUEST_BUNDLE_SHA256=${manifest.guestBundle.sha256}`,
  `SECONDBOX_RELEASE_EXPECTED_GUEST_BUNDLE_SIZE_BYTES=${manifest.guestBundle.sizeBytes}`,
);

const expectedProvenance = new Map([
  [
    "controlPlaneImage",
    [
      "SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE_PROVENANCE",
      "external-provenance/control-plane-image.intoto.json",
    ],
  ],
  [
    "runnerImage",
    [
      "SECONDBOX_RELEASE_RUNNER_IMAGE_PROVENANCE",
      "external-provenance/runner-image.intoto.json",
    ],
  ],
  [
    "guestExecutionBundle",
    [
      "SECONDBOX_RELEASE_GUEST_BUNDLE_PROVENANCE",
      "external-provenance/guest-execution-bundle.intoto.json",
    ],
  ],
  [
    "guestArtifactImage",
    [
      "SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE_PROVENANCE",
      "external-provenance/guest-artifact-image.intoto.json",
    ],
  ],
]);
for (const [field, [environmentName, expectedPath]] of expectedProvenance) {
  if (manifest.provenance?.[field] !== expectedPath) {
    fail(`${field} provenance path is not canonical`);
  }
  environmentLines.push(
    `${environmentName}=${path.join(evidenceDirectory, ...expectedPath.split("/"))}`,
  );
}

const canonicalRequiredPaths = [
  "dist/SHA256SUMS",
  "dist/secondbox",
  "dist/secondbox-artifact-evidence",
  "dist/secondbox-guest-agent",
  "dist/secondbox-runner",
  "dist/secondbox-runner-identity",
  "dist/secondboxd",
  `package/secondbox-${expectedVersion}-linux-amd64.SHA256SUMS`,
  `package/secondbox-${expectedVersion}-linux-amd64.manifest.json`,
  `package/secondbox-${expectedVersion}-linux-amd64.tar.gz`,
  `sdk/secondbox-${expectedVersion}-go-sdk.tar.gz`,
  `sdk/secondbox-${expectedVersion}-sdk.SHA256SUMS`,
  `sdk/secondstack-ai-secondbox-${expectedVersion}.tgz`,
  ...[...expectedProvenance.values()].map(([, relativePath]) => relativePath),
].sort();
const canonicalRequiredPathSet = new Set(canonicalRequiredPaths);
const canonicalArtifactPaths = [
  ...canonicalRequiredPaths,
  "protected-environment.json",
  "protected-workflow-identity.json",
  "release-candidate-inputs.json",
].sort();
const actualArtifactPaths = walkRegularFiles(sourceDirectory);
if (
  actualArtifactPaths.length !== canonicalArtifactPaths.length ||
  canonicalArtifactPaths.some(
    (relativePath, index) => actualArtifactPaths[index] !== relativePath,
  )
) {
  fail("candidate artifact contains missing, unexpected, or non-regular files");
}
const seenPaths = new Set();
const verifiedFiles = [];
for (const record of manifest.files) {
  if (
    typeof record?.path !== "string" ||
    !hasExactKeys(record, ["path", "sha256", "sizeBytes"]) ||
    !/^(?:dist|package|sdk|guest|external-provenance)\/[A-Za-z0-9._-]+$/.test(
      record.path,
    ) ||
    !canonicalRequiredPathSet.has(record.path) ||
    seenPaths.has(record.path) ||
    !/^[0-9a-f]{64}$/.test(record.sha256 ?? "") ||
    !Number.isSafeInteger(record.sizeBytes) ||
    record.sizeBytes < 1
  ) {
    fail(`candidate file record is invalid or duplicate: ${String(record?.path)}`);
  }
  seenPaths.add(record.path);
  const sourcePath = secureFile(
    path.join(sourceDirectory, ...record.path.split("/")),
    `candidate file ${record.path}`,
  );
  if (
    statSync(sourcePath).size !== record.sizeBytes ||
    createHash("sha256").update(readFileSync(sourcePath)).digest("hex") !==
      record.sha256
  ) {
    fail(`candidate file bytes drifted from manifest: ${record.path}`);
  }
  verifiedFiles.push({ record, sourcePath });
}
if (
  seenPaths.size !== canonicalRequiredPaths.length ||
  canonicalRequiredPaths.some((relativePath) => !seenPaths.has(relativePath))
) {
  fail("candidate manifest does not contain the canonical release input set");
}

for (const { record, sourcePath } of verifiedFiles) {
  const destinationPath = path.join(
    evidenceDirectory,
    ...record.path.split("/"),
  );
  ensureDirectory(path.dirname(destinationPath));
  try {
    lstatSync(destinationPath);
    fail(`candidate import refuses to overwrite: ${record.path}`);
  } catch (error) {
    if (error.code !== "ENOENT") {
      throw error;
    }
  }
  copyFileSync(sourcePath, destinationPath, fsConstants.COPYFILE_EXCL);
  if (
    statSync(destinationPath).size !== record.sizeBytes ||
    createHash("sha256")
      .update(readFileSync(destinationPath))
      .digest("hex") !== record.sha256
  ) {
    fail(`candidate import copied changed bytes for ${record.path}`);
  }
}

const githubEnvironmentStatus = lstatSync(githubEnvironmentPath);
if (
  !githubEnvironmentStatus.isFile() ||
  githubEnvironmentStatus.isSymbolicLink()
) {
  fail("GITHUB_ENV must be a regular non-symbolic-link file");
}
writeFileSync(
  githubEnvironmentPath,
  `${environmentLines.join("\n")}\n`,
  { encoding: "utf8", flag: "a" },
);
console.log(`SecondBox imported protected candidate inputs for ${expectedCommit}`);

function secureDirectory(directoryPath, label) {
  const resolvedPath = path.resolve(directoryPath);
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
}

function secureFile(filePath, label) {
  const resolvedPath = path.resolve(filePath);
  try {
    const status = lstatSync(resolvedPath);
    const canonicalPath = realpathSync(resolvedPath);
    if (
      !status.isFile() ||
      status.isSymbolicLink() ||
      canonicalPath !== resolvedPath ||
      !canonicalPath.startsWith(`${sourceDirectory}${path.sep}`)
    ) {
      fail(`${label} must be a canonical file inside candidate source`);
    }
    return canonicalPath;
  } catch (error) {
    fail(`${label} is unavailable: ${error.message}`);
  }
}

function ensureDirectory(directoryPath) {
  const relativePath = path.relative(evidenceDirectory, directoryPath);
  if (relativePath.startsWith("..") || path.isAbsolute(relativePath)) {
    fail("candidate destination escaped evidence directory");
  }
  let currentPath = evidenceDirectory;
  for (const component of relativePath.split(path.sep).filter(Boolean)) {
    currentPath = path.join(currentPath, component);
    try {
      const status = lstatSync(currentPath);
      if (
        !status.isDirectory() ||
        status.isSymbolicLink() ||
        realpathSync(currentPath) !== currentPath
      ) {
        fail(`candidate destination directory is unsafe: ${relativePath}`);
      }
    } catch (error) {
      if (error.code !== "ENOENT") {
        throw error;
      }
      mkdirSync(currentPath, { mode: 0o755 });
    }
  }
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
          `candidate artifact contains symbolic link: ${path.relative(rootPath, entryPath)}`,
        );
      }
      if (entry.isDirectory()) {
        pending.push(entryPath);
      } else if (entry.isFile()) {
        files.push(
          path.relative(rootPath, entryPath).split(path.sep).join("/"),
        );
      } else {
        fail(
          `candidate artifact contains non-regular file: ${path.relative(rootPath, entryPath)}`,
        );
      }
    }
  }
  return files.sort();
}

function hasExactKeys(value, expectedKeys) {
  if (
    value === null ||
    typeof value !== "object" ||
    Array.isArray(value)
  ) {
    return false;
  }
  const actualKeys = Object.keys(value).sort();
  return (
    actualKeys.length === expectedKeys.length &&
    expectedKeys
      .slice()
      .sort()
      .every((key, index) => actualKeys[index] === key)
  );
}

function fail(message) {
  console.error(`SecondBox release candidate import failed: ${message}`);
  process.exit(1);
}

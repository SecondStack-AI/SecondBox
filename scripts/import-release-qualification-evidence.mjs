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
} from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

if (process.argv.length !== 4) {
  console.error(
    "Usage: scripts/import-release-qualification-evidence.mjs SOURCE_DIRECTORY EVIDENCE_DIRECTORY",
  );
  process.exit(2);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const verifierPath = path.join(
  scriptDirectory,
  "verify-release-qualification-record.mjs",
);
const sourceDirectory = secureDirectory(
  process.argv[2],
  "qualification import source",
);
const evidenceDirectory = secureDirectory(
  process.argv[3],
  "release evidence directory",
);
const subjectManifestPath = path.join(
  evidenceDirectory,
  "release-subjects.json",
);
secureFile(subjectManifestPath, "release subject manifest");

const gates = [
  "kvm",
  "multi-runner",
  "durability",
  "data-plane",
  "network",
  "security",
];
const importedPaths = new Set();
const importedDigests = new Map();
let recordCount = 0;

for (const gate of gates) {
  const relativeRecordPath = `qualification/${gate}.json`;
  const recordPath = path.join(sourceDirectory, ...relativeRecordPath.split("/"));
  let recordLstat;
  try {
    recordLstat = lstatSync(recordPath);
  } catch (error) {
    if (error.code === "ENOENT") {
      continue;
    }
    throw error;
  }
  if (!recordLstat.isFile() || recordLstat.isSymbolicLink()) {
    fail(
      `SecondBox qualification import record is not a regular non-symbolic-link file: ${relativeRecordPath}`,
    );
  }
  const verification = spawnSync(
    process.execPath,
    [
      verifierPath,
      subjectManifestPath,
      recordPath,
      gate,
      sourceDirectory,
    ],
    { encoding: "utf8" },
  );
  if (verification.status !== 0) {
    process.stderr.write(verification.stderr);
    fail(`SecondBox qualification import rejected ${relativeRecordPath}`);
  }
  const recordContents = secureFile(
    recordPath,
    `qualification record ${relativeRecordPath}`,
  );
  const record = JSON.parse(recordContents);
  importedPaths.add(relativeRecordPath);
  importedDigests.set(relativeRecordPath, sha256(recordContents));
  for (const scenario of record.scenarios) {
    for (const artifact of scenario.artifacts) {
      if (!artifact.path.startsWith("qualification/")) {
        fail(
          `SecondBox qualification import artifact must remain under qualification/: ${artifact.path}`,
        );
      }
      const existingDigest = importedDigests.get(artifact.path);
      if (
        existingDigest !== undefined &&
        existingDigest !== artifact.sha256
      ) {
        fail(
          `SecondBox qualification import has conflicting digests for ${artifact.path}`,
        );
      }
      importedPaths.add(artifact.path);
      importedDigests.set(artifact.path, artifact.sha256);
    }
  }
  recordCount += 1;
}

if (recordCount === 0) {
  fail("SecondBox qualification import contains no recognized records");
}

const sourcePaths = walkRegularFiles(sourceDirectory);
for (const sourcePath of sourcePaths) {
  if (!importedPaths.has(sourcePath)) {
    fail(`SecondBox qualification import contains unreferenced file: ${sourcePath}`);
  }
}
for (const relativePath of importedPaths) {
  if (!sourcePaths.has(relativePath)) {
    fail(`SecondBox qualification import is missing referenced file: ${relativePath}`);
  }
  const destinationPath = path.join(
    evidenceDirectory,
    ...relativePath.split("/"),
  );
  try {
    lstatSync(destinationPath);
    fail(`SecondBox qualification import refuses to overwrite: ${relativePath}`);
  } catch (error) {
    if (error.code !== "ENOENT") {
      throw error;
    }
  }
}

for (const relativePath of [...importedPaths].sort()) {
  const sourcePath = path.join(sourceDirectory, ...relativePath.split("/"));
  const destinationPath = path.join(
    evidenceDirectory,
    ...relativePath.split("/"),
  );
  ensureDestinationDirectory(path.posix.dirname(relativePath));
  copyFileSync(sourcePath, destinationPath, fsConstants.COPYFILE_EXCL);
  const destinationDigest = sha256(readFileSync(destinationPath));
  if (destinationDigest !== importedDigests.get(relativePath)) {
    fail(
      `SecondBox qualification import copied changed bytes for ${relativePath}`,
    );
  }
}

console.log(
  `SecondBox imported ${recordCount} structured qualification record(s)`,
);

function fail(message) {
  console.error(message);
  process.exit(1);
}

function sha256(contents) {
  return createHash("sha256").update(contents).digest("hex");
}

function secureDirectory(directoryPath, label) {
  const resolvedPath = path.resolve(directoryPath);
  let directoryLstat;
  let canonicalPath;
  try {
    directoryLstat = lstatSync(resolvedPath);
    canonicalPath = realpathSync(resolvedPath);
  } catch (error) {
    fail(`SecondBox ${label} is unavailable: ${error.message}`);
  }
  if (
    !directoryLstat.isDirectory() ||
    directoryLstat.isSymbolicLink() ||
    canonicalPath !== resolvedPath
  ) {
    fail(`SecondBox ${label} must be a canonical non-symbolic-link directory`);
  }
  return canonicalPath;
}

function secureFile(filePath, label) {
  const resolvedPath = path.resolve(filePath);
  let fileLstat;
  let canonicalPath;
  try {
    fileLstat = lstatSync(resolvedPath);
    canonicalPath = realpathSync(resolvedPath);
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
  return readFileSync(canonicalPath, "utf8");
}

function walkRegularFiles(rootPath) {
  const files = new Set();
  const pending = [rootPath];
  while (pending.length > 0) {
    const directoryPath = pending.pop();
    for (const entry of readdirSync(directoryPath, { withFileTypes: true })) {
      const entryPath = path.join(directoryPath, entry.name);
      if (entry.isSymbolicLink()) {
        fail(
          `SecondBox qualification import contains symbolic link: ${path.relative(rootPath, entryPath)}`,
        );
      }
      if (entry.isDirectory()) {
        pending.push(entryPath);
        continue;
      }
      if (!entry.isFile()) {
        fail(
          `SecondBox qualification import contains non-regular file: ${path.relative(rootPath, entryPath)}`,
        );
      }
      const relativePath = path.relative(rootPath, entryPath).split(path.sep).join("/");
      files.add(relativePath);
    }
  }
  return files;
}

function ensureDestinationDirectory(relativeDirectory) {
  let currentPath = evidenceDirectory;
  for (const component of relativeDirectory.split("/")) {
    currentPath = path.join(currentPath, component);
    try {
      const directoryLstat = lstatSync(currentPath);
      if (
        !directoryLstat.isDirectory() ||
        directoryLstat.isSymbolicLink() ||
        realpathSync(currentPath) !== currentPath
      ) {
        fail(
          `SecondBox qualification import destination directory is unsafe: ${relativeDirectory}`,
        );
      }
    } catch (error) {
      if (error.code !== "ENOENT") {
        throw error;
      }
      mkdirSync(currentPath, { mode: 0o755 });
    }
  }
}

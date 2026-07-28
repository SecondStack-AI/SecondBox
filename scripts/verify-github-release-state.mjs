#!/usr/bin/env node

import { readFileSync } from "node:fs";

if (process.argv.length !== 8) {
  console.error(
    "Usage: scripts/verify-github-release-state.mjs IMMUTABLE_CONFIG.json|- RELEASES.json ASSETS.json EXPECTED_ASSET_NAMES.json RELEASE_TAG PHASE",
  );
  process.exit(2);
}

const immutableConfigurationPath = process.argv[2];
const releases = readJSON(process.argv[3], "GitHub releases");
const assets = readJSON(process.argv[4], "GitHub release assets");
const expectedAssetNames = readJSON(
  process.argv[5],
  "expected GitHub release asset names",
);
const releaseTag = process.argv[6];
const phase = process.argv[7];

if (!["before-upload", "after-upload", "public"].includes(phase)) {
  fail(`unknown release-state phase: ${phase}`);
}
if (!/^v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(releaseTag)) {
  fail("release tag must be canonical SemVer with a leading v");
}
if (phase !== "public") {
  const immutableConfiguration = readJSON(
    immutableConfigurationPath,
    "immutable-release configuration",
  );
  if (immutableConfiguration.enabled !== true) {
    fail("repository immutable releases are not enabled");
  }
} else if (immutableConfigurationPath !== "-") {
  fail("public verification must not trust a caller-authored configuration file");
}
if (!Array.isArray(releases) || !Array.isArray(assets)) {
  fail("GitHub release or asset inventory is not an array");
}
if (
  !Array.isArray(expectedAssetNames) ||
  expectedAssetNames.length === 0 ||
  expectedAssetNames.some(
    (name) =>
      typeof name !== "string" ||
      name.length === 0 ||
      name.includes("/") ||
      name === "." ||
      name === "..",
  ) ||
  new Set(expectedAssetNames).size !== expectedAssetNames.length
) {
  fail("expected GitHub release asset inventory is invalid or duplicate");
}

const matchingReleases = releases.filter(
  (release) => release?.tag_name === releaseTag,
);
if (matchingReleases.length > 1) {
  fail(`multiple draft or public releases exist for ${releaseTag}`);
}
if (phase !== "before-upload" && matchingReleases.length !== 1) {
  fail(`${phase} requires exactly one release for ${releaseTag}`);
}
if (matchingReleases.length === 0) {
  if (assets.length !== 0) {
    fail("asset inventory was supplied without a matching release");
  }
  console.log(`SecondBox GitHub release state permits creating ${releaseTag}`);
  process.exit(0);
}

const release = matchingReleases[0];
if (
  !Number.isSafeInteger(release.id) ||
  release.id < 1 ||
  typeof release.draft !== "boolean" ||
  release.prerelease !== false
) {
  fail("matching GitHub release identity is malformed or prerelease");
}
if (phase === "public") {
  if (release.draft !== false || release.immutable !== true) {
    fail("public GitHub release is not stable and immutable");
  }
} else if (release.draft === false && release.immutable !== true) {
  fail("existing public GitHub release is not immutable");
}

const expectedNames = new Set(expectedAssetNames);
const actualNames = [];
for (const asset of assets) {
  if (
    !Number.isSafeInteger(asset?.id) ||
    asset.id < 1 ||
    typeof asset.name !== "string" ||
    asset.name.length === 0 ||
    asset.state !== "uploaded"
  ) {
    fail("GitHub release contains a malformed or incomplete asset");
  }
  actualNames.push(asset.name);
}
if (new Set(actualNames).size !== actualNames.length) {
  fail("GitHub release contains duplicate asset names");
}
for (const actualName of actualNames) {
  if (!expectedNames.has(actualName)) {
    fail(`GitHub release contains unexpected asset ${actualName}`);
  }
}
if (
  phase === "after-upload" ||
  phase === "public" ||
  release.draft === false
) {
  for (const expectedName of expectedNames) {
    if (!actualNames.includes(expectedName)) {
      fail(`GitHub release is missing expected asset ${expectedName}`);
    }
  }
}

console.log(
  `SecondBox GitHub release state is valid for ${releaseTag} during ${phase}`,
);

function readJSON(documentPath, label) {
  try {
    return JSON.parse(readFileSync(documentPath, "utf8"));
  } catch (error) {
    fail(`${label} could not be decoded: ${error.message}`);
  }
}

function fail(message) {
  console.error(`SecondBox GitHub release state invalid: ${message}`);
  process.exit(1);
}

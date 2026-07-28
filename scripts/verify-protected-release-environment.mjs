#!/usr/bin/env node

import { readFileSync } from "node:fs";

if (process.argv.length !== 4) {
  console.error(
    "Usage: scripts/verify-protected-release-environment.mjs ENVIRONMENT.json EXPECTED_NAME",
  );
  process.exit(2);
}

const environment = readJSON(process.argv[2]);
const expectedName = process.argv[3];
if (
  ![
    "release-candidate",
    "release-qualification",
    "release-evidence",
    "release",
  ].includes(expectedName)
) {
  fail(`unknown protected release environment: ${expectedName}`);
}
const reviewerRules = Array.isArray(environment.protection_rules)
  ? environment.protection_rules.filter(
      (rule) => rule?.type === "required_reviewers",
    )
  : [];
const branchPolicyRules = Array.isArray(environment.protection_rules)
  ? environment.protection_rules.filter(
      (rule) => rule?.type === "branch_policy",
    )
  : [];
if (
  environment.name !== expectedName ||
  reviewerRules.length !== 1 ||
  reviewerRules[0].prevent_self_review !== true ||
  !Array.isArray(reviewerRules[0].reviewers) ||
  reviewerRules[0].reviewers.length < 1 ||
  branchPolicyRules.length !== 1 ||
  environment.deployment_branch_policy?.protected_branches !== true ||
  environment.deployment_branch_policy?.custom_branch_policies !== false
) {
  fail(
    `${expectedName} must require a reviewer, prevent self-review, and permit only protected branches`,
  );
}
console.log(`SecondBox verified protected release environment ${expectedName}`);

function readJSON(documentPath) {
  try {
    return JSON.parse(readFileSync(documentPath, "utf8"));
  } catch (error) {
    fail(`environment response could not be decoded: ${error.message}`);
  }
}

function fail(message) {
  console.error(`SecondBox protected release environment invalid: ${message}`);
  process.exit(1);
}

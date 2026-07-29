#!/usr/bin/env bash
set -euo pipefail

: "${SECONDBOX_TEST_DATABASE_URL:?SecondBox multi-runner tests require SECONDBOX_TEST_DATABASE_URL to target a disposable PostgreSQL test database}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
require_qualified="${SECONDBOX_REQUIRE_QUALIFIED_MULTIRUNNER:-0}"

if [[ "$require_qualified" != "0" && "$require_qualified" != "1" ]]; then
  echo "SECONDBOX_REQUIRE_QUALIFIED_MULTIRUNNER must be 0 or 1" >&2
  exit 1
fi

if [[ "$require_qualified" == "1" ]]; then
  : "${SECONDBOX_MULTIRUNNER_RUNNER_A_ID:?qualified multi-runner tests require runner A stable identity}"
  : "${SECONDBOX_MULTIRUNNER_RUNNER_B_ID:?qualified multi-runner tests require runner B stable identity}"
  : "${SECONDBOX_MULTIRUNNER_FILESYSTEM_A:?qualified multi-runner tests require runner A reflink filesystem path}"
  : "${SECONDBOX_MULTIRUNNER_FILESYSTEM_B:?qualified multi-runner tests require runner B reflink filesystem path}"
  if [[ "$SECONDBOX_MULTIRUNNER_RUNNER_A_ID" == "$SECONDBOX_MULTIRUNNER_RUNNER_B_ID" ]]; then
    echo "Qualified multi-runner stable identities must be distinct" >&2
    exit 1
  fi
  if [[ "$SECONDBOX_MULTIRUNNER_FILESYSTEM_A" == "$SECONDBOX_MULTIRUNNER_FILESYSTEM_B" ]]; then
    echo "Qualified multi-runner workspace filesystems must be distinct paths" >&2
    exit 1
  fi
  for command in findmnt git go; do
    if ! command -v "$command" >/dev/null 2>&1; then
      echo "Qualified multi-runner prerequisite missing: $command" >&2
      exit 1
    fi
  done
  for root in "$SECONDBOX_MULTIRUNNER_FILESYSTEM_A" "$SECONDBOX_MULTIRUNNER_FILESYSTEM_B"; do
    if [[ "$root" != /* || "$root" == "/" || ! -d "$root" ]]; then
      echo "Qualified multi-runner filesystem must be an existing non-root absolute directory: $root" >&2
      exit 1
    fi
    filesystem_type="$(findmnt --target "$root" --noheadings --output FSTYPE | xargs)"
    if [[ "$filesystem_type" != "xfs" && "$filesystem_type" != "btrfs" ]]; then
      echo "Qualified multi-runner filesystem $root uses unsupported type $filesystem_type" >&2
      exit 1
    fi
    findmnt --target "$root" --noheadings --output TARGET,SOURCE,FSTYPE,OPTIONS
  done
fi

cd "$repo_root"
go test ./tests/integration \
  -run '^TestTwoFakeRunnersPinHomesAndNeverRelocate$' \
  -count=1

cd "$repo_root/runner"
go test ./internal/workspacestore \
  -run '^TestQualifiedTwoRunnerRootsAreDistinctAndIsolated$' \
  -count=1 -v

source_commit="$(git -C "$repo_root" rev-parse HEAD)"
echo "SecondBox multi-runner tests passed for commit $source_commit with $(go version)"

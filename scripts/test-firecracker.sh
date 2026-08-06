#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$repo_root/runner"
# Firecracker validation is explicit and never skips: without this gate every
# targeted test t.Skips on an unqualified machine and the suite passes vacuously.
export SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1
exec go test ./internal/firecracker \
  -run '^(TestSmokeBootFirecracker|TestSmokeRunnerLocalSnapshotRestore|TestSmokeRunnerLocalLifecycleStopPaths)$' \
  -count=1

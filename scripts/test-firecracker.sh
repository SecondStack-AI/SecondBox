#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$repo_root/runner"
exec go test ./internal/firecracker \
  -run '^(TestSmokeBootFirecracker|TestSmokeRunnerLocalSnapshotRestore|TestSmokeRunnerLocalLifecycleStopPaths)$' \
  -count=1

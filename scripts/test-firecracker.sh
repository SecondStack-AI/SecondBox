#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$repo_root/runner"
# Firecracker validation is explicit and never skips: without this gate every
# targeted test t.Skips on an unqualified machine and the suite passes vacuously.
export SECONDBOX_RUNNER_QUALIFY_FIRECRACKER=1
go test ./internal/firecracker \
  -run '^(TestSmokeBootFirecracker|TestSmokeRunnerLocalSnapshotRestore|TestSmokeRunnerLocalLifecycleStopPaths)$' \
  -count=1

export SECONDBOX_RUNNER_QUALIFY_TENANT_EGRESS=1
exec unshare --user --map-root-user --net \
  go test ./internal/firecracker \
  -run '^TestSmokeTenantEgressContextIsolation$' \
  -count=1

#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$repo_root"
go test ./sdk/go/secondboxclient ./tests/contract ./tests/protocol -count=1

cd "$repo_root/runner"
go test ./internal/guest -count=1
go test ./internal/firecracker -count=1 -run \
  '^(TestControlClient|TestNegotiateGuestProtocol|TestToolExecutor|TestSandboxHostHTTPContract|TestSandboxHostAllowedConnectionIDs|TestSandboxHostStopRequiresDurableWorkspaceFreeze|TestWorkspaceEvidenceClonePreservesSparseAllocation)'

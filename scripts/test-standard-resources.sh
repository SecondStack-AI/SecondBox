#!/usr/bin/env bash
set -euo pipefail

: "${SECONDBOX_TEST_DATABASE_URL:?SecondBox standard-resource qualification requires a disposable PostgreSQL test database}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

go test ./pkg/resourceapply ./pkg/standardresources ./cmd/secondbox ./cmd/secondbox-deploy ./internal/deployconfig -count=1
go test ./tests/integration -run '^TestStandardResourcesFreshUpgradeAndReplayConvergeThroughLiveControlPlane$' -count=1

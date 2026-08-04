#!/usr/bin/env bash
set -euo pipefail

: "${SECONDBOX_TEST_DATABASE_URL:?SecondBox Go tests require SECONDBOX_TEST_DATABASE_URL to target a disposable PostgreSQL test database}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$repo_root"
go test ./... -count=1
go vet ./...
go vet -tags "scenario_live sdk_live" ./...
GOOS=darwin CGO_ENABLED=0 go vet ./internal/deployconfig

cd "$repo_root/runner"
scripts/ci_paths_test.sh
go test ./... -count=1
go vet ./...

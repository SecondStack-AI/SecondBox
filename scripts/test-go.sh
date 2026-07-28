#!/usr/bin/env bash
set -euo pipefail

: "${SECONDBOX_TEST_DATABASE_URL:?SecondBox Go tests require SECONDBOX_TEST_DATABASE_URL to target a disposable PostgreSQL test database}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cd "$repo_root"
go test ./... -count=1
go vet ./...
python3 -c 'import ast, pathlib; ast.parse(pathlib.Path("sdk/python/secondbox_client_gen.py").read_text())'

cd "$repo_root/runner"
scripts/ci_paths_test.sh
go test ./... -count=1
go vet ./...

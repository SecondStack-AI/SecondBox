# Sandbox Service build and validation tasks.

default:
    @just --list

generate:
    go run ./cmd/generate-clients

verify-generated:
    go run ./cmd/generate-clients --check

build:
    go build -o /dev/null ./cmd/sandbox-service

test:
    #!/usr/bin/env bash
    set -euo pipefail
    : "${SANDBOX_SERVICE_TEST_DATABASE_URL:?SANDBOX_SERVICE_TEST_DATABASE_URL must target a disposable PostgreSQL test database}"
    go run ./cmd/generate-clients --check
    go test ./... -count=1
    go vet ./...
    python3 -c 'import ast, pathlib; ast.parse(pathlib.Path("gen/python/sandbox_client_gen.py").read_text())'

preship: test build

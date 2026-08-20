#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
generated="$repo_root/runner/internal/microsandboxprotocol/helper.pb.go"
schema="$repo_root/contracts/microsandbox-helper/v1/helper.proto"

[[ -f "$generated" ]] || { echo "SecondBox Microsandbox helper Go binding is missing" >&2; exit 1; }
command -v protoc >/dev/null 2>&1 || {
  echo "protoc is required to verify the Microsandbox helper protocol" >&2
  exit 1
}
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
GOBIN="$temporary/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
mkdir -p "$temporary/runner"
PATH="$temporary/bin:$PATH" protoc --proto_path="$repo_root/contracts" \
  --go_out="$temporary/runner" \
  --go_opt=module=github.com/SecondStack-AI/SecondBox/runner \
  "$schema"
diff -u "$generated" "$temporary/runner/internal/microsandboxprotocol/helper.pb.go"

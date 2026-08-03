#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

work_dir="$(mktemp -d)"
cleanup() {
    rm -rf "$work_dir"
}
trap cleanup EXIT

go run ./cmd/secondbox-sdkgen -output-root "$work_dir"

for relative_path in \
    sdk/go/secondboxclient/transport_generated.go \
    sdk/go/secondboxclient/wire_types_generated.go \
    sdk/typescript/public-surface.json \
    sdk/typescript/transport.generated.ts; do
    cmp -s "$work_dir/$relative_path" "$relative_path" || {
        echo "generated SDK transport is stale: $relative_path" >&2
        exit 1
    }
done

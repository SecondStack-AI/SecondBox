#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

command -v protoc >/dev/null 2>&1 || {
    echo "protoc is required to verify the canonical guest protocol" >&2
    exit 1
}

work_dir="$(mktemp -d)"
cleanup() {
    rm -rf "$work_dir"
}
trap cleanup EXIT

GOBIN="$work_dir/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
GOBIN="$work_dir/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1

mkdir -p "$work_dir/generated"
PATH="$work_dir/bin:$PATH" protoc \
    --go_out="$work_dir/generated" \
    --go_opt=module=github.com/SecondStack-AI/SecondBox \
    --go-grpc_out="$work_dir/generated" \
    --go-grpc_opt=module=github.com/SecondStack-AI/SecondBox \
    contracts/guest/v1/guest.proto

for name in guest.pb.go guest_grpc.pb.go; do
    generated="$work_dir/generated/gen/guest/v1/$name"
    cmp "$generated" "gen/guest/v1/$name" || {
        echo "root generated guest protocol is stale: gen/guest/v1/$name" >&2
        exit 1
    }
    cmp "$generated" "runner/internal/guestprotocol/$name" || {
        echo "runner generated guest protocol is stale: runner/internal/guestprotocol/$name" >&2
        exit 1
    }
done

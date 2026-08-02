#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

command -v protoc >/dev/null 2>&1 || {
    echo "protoc is required to verify the canonical runner protocol" >&2
    exit 1
}

work_dir="$(mktemp -d)"
cleanup() {
    rm -rf "$work_dir"
}
trap cleanup EXIT

GOBIN="$work_dir/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
GOBIN="$work_dir/bin" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2

mkdir -p "$work_dir/generated"
PATH="$work_dir/bin:$PATH" protoc \
    --go_out="$work_dir/generated" \
    --go_opt=module=github.com/SecondStack-AI/SecondBox \
    --go-grpc_out="$work_dir/generated" \
    --go-grpc_opt=module=github.com/SecondStack-AI/SecondBox \
    --descriptor_set_out="$work_dir/runner.descriptor.pb" \
    contracts/runner/v1/runner.proto

for name in runner.pb.go runner_grpc.pb.go; do
    generated="$work_dir/generated/gen/runner/v1/$name"
    cmp "$generated" "gen/runner/v1/$name" || {
        echo "root generated runner protocol is stale: gen/runner/v1/$name" >&2
        exit 1
    }
    cmp "$generated" "runner/internal/runnerprotocol/$name" || {
        echo "runner generated protocol is stale: runner/internal/runnerprotocol/$name" >&2
        exit 1
    }
done

cmp gen/runner/v1/version.go runner/internal/runnerprotocol/version.go >/dev/null || {
    # Package declarations and comments intentionally differ, so compare the
    # authoritative declarations rather than requiring byte-identical files.
    root_window="$(grep -E 'SupportedProtocol(Minimum|Maximum).*=' gen/runner/v1/version.go)"
    runner_window="$(grep -E 'SupportedProtocol(Minimum|Maximum).*=' runner/internal/runnerprotocol/version.go)"
    [[ "$root_window" == "$runner_window" ]] || {
        echo "runner protocol version constants drifted between modules" >&2
        exit 1
    }
}

cmp "$work_dir/runner.descriptor.pb" contracts/runner/v1/runner.descriptor.pb || {
    echo "frozen runner protocol descriptor is stale" >&2
    exit 1
}
(
    cd contracts/runner/v1
    sha256sum -c runner.descriptor.sha256
)

#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="$repo_root/dist"

mkdir -p "$output_dir"

cd "$repo_root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$output_dir/secondbox" ./cmd/secondbox
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$output_dir/secondbox-deploy" ./cmd/secondbox-deploy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$output_dir/secondboxd" ./cmd/secondboxd

cd "$repo_root/runner"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$output_dir/secondbox-runner" ./cmd/secondbox-runner
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$output_dir/secondbox-guest-agent" ./cmd/secondbox-guest-agent
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o "$output_dir/secondbox-artifact-evidence" ./cmd/secondbox-artifact-evidence

(
  cd "$output_dir"
  sha256sum \
    secondbox \
    secondbox-artifact-evidence \
    secondbox-deploy \
    secondbox-guest-agent \
    secondbox-runner \
    secondboxd > SHA256SUMS
)

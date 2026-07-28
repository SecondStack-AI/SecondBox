#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

"$repo_root/runner/scripts/microvm-image/browser-surface-policy-test.sh"
"$repo_root/runner/scripts/microvm-image/trust-anchor-policy-test.sh"
"$repo_root/scripts/test-source-asset-policy.sh"
cd "$repo_root"
go test ./tests/image-pipeline -count=1

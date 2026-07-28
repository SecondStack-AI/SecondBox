#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

npm ci --ignore-scripts
scripts/verify-generated.sh
scripts/test-go.sh
scripts/test-contract.sh
scripts/test-compose.sh
scripts/test-image-policy.sh
scripts/build-artifacts.sh

source_commit="$(git rev-parse HEAD)"
echo "SecondBox non-KVM qualification passed for commit $source_commit"

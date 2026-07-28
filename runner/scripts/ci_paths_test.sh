#!/usr/bin/env bash
set -euo pipefail

runner_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo_root="$(cd "$runner_root/.." && pwd)"
standalone_ci="$repo_root/.github/workflows/ci.yml"

if [[ ! -f "$standalone_ci" ]]; then
    echo "SecondBox standalone CI workflow is missing: $standalone_ci" >&2
    exit 1
fi
for job in generated-checks go-tests-vet contract-tests compose-tests image-policy build-artifacts; do
    if ! grep -Eq "^  ${job}:" "$standalone_ci"; then
        echo "SecondBox standalone CI workflow is missing job $job" >&2
        exit 1
    fi
done
if ! grep -Eq '^test-firecracker:' "$repo_root/Justfile"; then
    echo "SecondBox root Justfile is missing test-firecracker" >&2
    exit 1
fi
if ! grep -Eq '^test-multirunner:' "$repo_root/Justfile"; then
    echo "SecondBox root Justfile is missing test-multirunner" >&2
    exit 1
fi

echo "SecondBox standalone CI and qualified-host entry points are discoverable"

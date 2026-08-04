#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="$repo_root/.github/workflows/release.yml"
scenario="$repo_root/.github/workflows/scenario-qualification.yml"

bash -n "$repo_root/scripts/release-publish-candidate.sh"
go -C "$repo_root" test ./pkg/releasepublish ./pkg/releasecontract -count=1
"$repo_root/scripts/test-source-free-release-contract.sh"

rg -q 'tags:' "$workflow"
rg -q 'prepublication-kvm:' "$workflow"
rg -q 'needs: prepublication-kvm' "$workflow"
rg -q 'verify-candidate-evidence' "$workflow"
rg -q -- '--tag candidate --provenance' "$repo_root/scripts/release-publish-candidate.sh"
rg -q -- '--draft=false --prerelease' "$repo_root/scripts/release-publish-candidate.sh"
rg -q 'candidate-kvm-evidence' "$scenario"

for dockerfile in "$repo_root/Dockerfile" "$repo_root/runner/Dockerfile" "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile"; do
  while IFS= read -r base; do
    [[ "$base" == "scratch" || "$base" == *@sha256:* ]] || { echo "release Dockerfile uses mutable base $base" >&2; exit 1; }
  done < <(awk '$1 == "FROM" {print $2}' "$dockerfile")
done

if rg -n '^\s*(-\s+)?uses:\s*[^ ]+@(main|master|v[0-9]+([.]?[0-9]+)*)\s*$' "$workflow" "$scenario"; then
  echo "release workflow contains an unpinned external action" >&2
  exit 1
fi
if ! rg -q 'must not include final qualification or release-index authority' "$repo_root/scripts/release-publish-candidate.sh"; then
  echo "candidate publisher can publish final release authority" >&2
  exit 1
fi

#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="$repo_root/.github/workflows/release.yml"

for script in \
  release-publish-candidate.sh \
  release-hosted-publish.sh \
  release-local-prepare.sh \
  release-local-upload.sh \
  release-local-qualify.sh \
  release-local-finalize.sh \
  release-verify-public-candidate.sh; do
  bash -n "$repo_root/scripts/$script"
done
go -C "$repo_root" test ./pkg/releasepublish ./pkg/releasecontract -count=1
"$repo_root/scripts/test-source-free-release-contract.sh"

rg -q 'workflow_dispatch:' "$workflow"
rg -q 'runs-on: ubuntu-latest' "$workflow"
rg -q 'verify-publication-input' "$repo_root/scripts/release-hosted-publish.sh"
rg -q 'release-hosted-publish.sh publish' "$workflow"
rg -q 'release-hosted-publish.sh expose' "$workflow"
rg -q 'attest-build-provenance@' "$workflow"
if rg -q 'self-hosted|secondbox-kvm|release-stage|docker build|go build|npm pack' "$workflow"; then
  echo "hosted candidate publisher rebuilds or requires a self-hosted Runner" >&2
  exit 1
fi
rg -q -- '--tag candidate --provenance' "$repo_root/scripts/release-publish-candidate.sh"
rg -q -- '--defer-release' "$repo_root/scripts/release-hosted-publish.sh"
rg -q -- '--draft=false --prerelease' "$repo_root/scripts/release-hosted-publish.sh"
test ! -e "$repo_root/.github/workflows/scenario-qualification.yml"
test ! -e "$repo_root/.github/workflows/release-finalize.yml"

for dockerfile in "$repo_root/Dockerfile" "$repo_root/runner/Dockerfile" "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile"; do
  while IFS= read -r base; do
    [[ "$base" == "scratch" || "$base" == *@sha256:* ]] || { echo "release Dockerfile uses mutable base $base" >&2; exit 1; }
  done < <(awk '$1 == "FROM" {print $2}' "$dockerfile")
done

if rg -n '^\s*(-\s+)?uses:\s*[^ ]+@(main|master|v[0-9]+([.]?[0-9]+)*)\s*$' "$workflow"; then
  echo "release workflow contains an unpinned external action" >&2
  exit 1
fi
if ! rg -q 'must not include final qualification or release-index authority' "$repo_root/scripts/release-publish-candidate.sh"; then
  echo "candidate publisher can publish final release authority" >&2
  exit 1
fi
if ! rg -q 'transport manifest last' "$repo_root/scripts/release-local-upload.sh"; then
  echo "local uploader does not make draft completeness explicit" >&2
  exit 1
fi

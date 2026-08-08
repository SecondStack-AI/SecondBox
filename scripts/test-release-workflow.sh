#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
workflow="$repo_root/.github/workflows/release.yml"
stager="$repo_root/scripts/release-stage.sh"
uploader="$repo_root/scripts/release-upload.sh"
publisher="$repo_root/scripts/release-publish.sh"

bash -n "$uploader"
bash -n "$publisher"

rg -q 'workflow_dispatch:' "$workflow"
rg -q 'runs-on: ubuntu-latest' "$workflow"
rg -q 'scripts/release-publish.sh' "$workflow"
rg -q -- '--tag latest --provenance' "$publisher"
rg -q -- '--draft=false' "$publisher"
rg -q -- '--prerelease=false' "$publisher"
rg -q -- '--latest' "$publisher"
rg -q 'gh workflow run release.yml' "$uploader"
rg -q 'qualification-evidence' "$stager"
rg -q '^export LC_ALL=C$' "$stager"

if rg -q 'qualif|/dev/kvm|test-scenario|self-hosted' "$repo_root/.github/workflows"; then
  echo "GitHub workflows contain a forbidden qualification step" >&2
  exit 1
fi
if rg -q 'qualif|/dev/kvm|test-scenario|self-hosted' "$uploader" "$publisher"; then
  echo "hosted release publication contains a forbidden qualification step" >&2
  exit 1
fi
if rg -q 'qualification-attestation|attest-build-provenance|candidate-evidence|publication-input|release-index|verify-publication' \
  "$stager" "$workflow" "$uploader" "$publisher"; then
  echo "release flow contains a removed candidate, attestation, or finalization surface" >&2
  exit 1
fi
if rg -q 'release-stage|docker build|go build|npm pack' "$workflow"; then
  echo "GitHub publisher rebuilds locally supplied artifacts" >&2
  exit 1
fi
if rg -q 'qualification-attestation|release-index|candidate-evidence|publication-input|verify-publication' \
  "$repo_root/cmd/secondbox-deploy/main.go" "$repo_root/cmd/secondbox-release-tool/main.go"; then
  echo "release CLIs retain a removed candidate or finalization command" >&2
  exit 1
fi
test ! -e "$repo_root/pkg/releasefinalize"
test ! -e "$repo_root/pkg/releasepublish"

for dockerfile in "$repo_root/Dockerfile" "$repo_root/deploy/installer-tools.Dockerfile" "$repo_root/runner/Dockerfile" "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile"; do
  while IFS= read -r base; do
    [[ "$base" == "scratch" || "$base" == *@sha256:* ]] || { echo "release Dockerfile uses mutable base $base" >&2; exit 1; }
  done < <(awk '$1 == "FROM" {print $2}' "$dockerfile")
done

if rg -n '^\s*(-\s+)?uses:\s*[^ ]+@(main|master|v[0-9]+([.]?[0-9]+)*)\s*$' "$workflow"; then
  echo "release workflow contains an unpinned external action" >&2
  exit 1
fi

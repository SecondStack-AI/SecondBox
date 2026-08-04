#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: scripts/release-local-prepare.sh [--test-mode] VERSION OUTPUT_DIR" >&2
  exit 2
}

test_mode=false
if [[ "${1:-}" == "--test-mode" ]]; then
  test_mode=true
  shift
fi
[[ "$#" -eq 2 ]] || usage
version="$1"
output_root="$2"
tag="v${version}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || {
  echo "local release preparation requires canonical SemVer without a v prefix" >&2
  exit 1
}
[[ ! -e "$output_root" ]] || { echo "local release output already exists: $output_root" >&2; exit 1; }

if ! $test_mode; then
  [[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]] || { echo "local release preparation requires a clean repository" >&2; exit 1; }
  tag_commit="$(git -C "$repo_root" rev-parse "refs/tags/${tag}^{commit}" 2>/dev/null)" || { echo "local release preparation requires immutable tag $tag" >&2; exit 1; }
  source_commit="$(git -C "$repo_root" rev-parse HEAD)"
  [[ "$tag_commit" == "$source_commit" ]] || { echo "local release tag $tag does not identify HEAD $source_commit" >&2; exit 1; }
  : "${SECONDBOX_TEST_DATABASE_URL:?local release preparation requires SECONDBOX_TEST_DATABASE_URL}"
  just --justfile "$repo_root/Justfile" --working-directory "$repo_root" test-non-kvm
  just --justfile "$repo_root/Justfile" --working-directory "$repo_root" test-release-stage
  just --justfile "$repo_root/Justfile" --working-directory "$repo_root" test-release-workflow
  just --justfile "$repo_root/Justfile" --working-directory "$repo_root" test-scenario
fi

mkdir -p "$output_root"
output_root="$(cd "$output_root" && pwd)"
candidate="$output_root/candidate"
if $test_mode; then
  touch "$output_root/.test-mode"
  "$repo_root/scripts/release-stage.sh" --test-mode "$version" "$candidate"
  runner_environment="sha256:$(printf '%s' '{"mode":"synthetic-local-release"}' | sha256sum | awk '{print $1}')"
else
  "$repo_root/scripts/release-stage.sh" "$version" "$candidate"
  runner_environment="sha256:$(jq -cn \
    --arg kernel "$(uname -srmo)" \
    --arg host "$(hostname)" \
    --arg architecture "$(uname -m)" \
    --arg kvm "$(stat -c '%t:%T:%A' /dev/kvm)" \
    '{kernel:$kernel,host:$host,architecture:$architecture,kvm:$kvm}' | sha256sum | awk '{print $1}')"
fi

manifest="$candidate/secondbox-${version}-artifact-manifest.json"
evidence="$output_root/secondbox-${version}-candidate-kvm-evidence.json"
publication_input="$output_root/secondbox-${version}-publication-input.json"
go -C "$repo_root" run ./cmd/secondbox-release-tool candidate-evidence "$manifest" "$runner_environment" "$evidence"
go -C "$repo_root" run ./cmd/secondbox-release-tool publication-input "$candidate" "$evidence" "$publication_input"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify-publication-sources "$candidate" "$evidence" "$publication_input"

jq -n \
  --arg version "$version" \
  --arg tag "$tag" \
  --arg output "$output_root" \
  --arg manifest "sha256:$(sha256sum "$manifest" | awk '{print $1}')" \
  '{version:$version,tag:$tag,output:$output,artifactManifestDigest:$manifest,state:"prepared-local-publication-input"}'

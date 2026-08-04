#!/usr/bin/env bash
set -euo pipefail

dry_run=false
if [[ "${1:-}" == "--dry-run" ]]; then
  dry_run=true
  shift
fi
[[ "$#" -eq 2 ]] || { echo "usage: scripts/release-local-upload.sh [--dry-run] VERSION PREPARED_DIR" >&2; exit 2; }
version="$1"
prepared="$2"
tag="v${version}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
candidate="$prepared/candidate"
evidence="$prepared/secondbox-${version}-candidate-kvm-evidence.json"
publication_input="$prepared/secondbox-${version}-publication-input.json"

[[ ! -e "$prepared/.test-mode" ]] || { echo "local release upload refuses synthetic test-mode output" >&2; exit 1; }

go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$candidate"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify-candidate-evidence "$candidate/secondbox-${version}-artifact-manifest.json" "$evidence"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify-publication-sources "$candidate" "$evidence" "$publication_input"
source_commit="$(jq -er '.sourceCommit' "$publication_input")"
tag_commit="$(git -C "$repo_root" rev-parse "refs/tags/${tag}^{commit}" 2>/dev/null)" || { echo "local release upload requires immutable tag $tag" >&2; exit 1; }
[[ "$tag_commit" == "$source_commit" ]] || { echo "local release upload tag and publication input identify different commits" >&2; exit 1; }

if $dry_run; then
  jq -n --arg version "$version" --arg tag "$tag" --argjson files "$(jq '.files | length' "$publication_input")" '{mode:"dry-run",version:$version,tag:$tag,transportFiles:$files,state:"ready-for-draft-upload"}'
  exit 0
fi

gh auth status >/dev/null
[[ "$(gh repo view --json nameWithOwner --jq .nameWithOwner)" == "SecondStack-AI/SecondBox" ]] || { echo "local release upload is authenticated against the wrong repository" >&2; exit 1; }
release_policy="$(gh api -H 'X-GitHub-Api-Version: 2026-03-10' 'repos/SecondStack-AI/SecondBox/immutable-releases')"
jq -e '.enabled == false' <<<"$release_policy" >/dev/null || {
  echo "SecondBox staged release finalization requires GitHub native release immutability to be disabled" >&2
  exit 1
}
if gh release view "$tag" --json isDraft,isPrerelease >/dev/null 2>&1; then
  state="$(gh release view "$tag" --json isDraft,isPrerelease)"
  jq -e '.isDraft == true and .isPrerelease == false' <<<"$state" >/dev/null || { echo "local release upload refuses a non-draft release" >&2; exit 1; }
else
  gh release create "$tag" --draft --verify-tag --title "SecondBox $tag" --notes "Private local-build transport. This draft is not release authority."
fi

declare -A expected=()
while IFS= read -r name; do expected["$name"]=1; done < <(jq -r '.files[].name' "$publication_input")
expected["${publication_input##*/}"]=1
while IFS= read -r existing_name; do
  [[ -n "${expected[$existing_name]:-}" ]] || { echo "draft release contains unknown transport asset $existing_name" >&2; exit 1; }
done < <(gh release view "$tag" --json assets --jq '.assets[].name')

cache="$(mktemp -d)"
cleanup() { rm -rf "$cache"; }
trap cleanup EXIT
upload_exact() {
  local path="$1"
  local name="${path##*/}"
  local existing="$cache/$name"
  if gh release download "$tag" --pattern "$name" --output "$existing" >/dev/null 2>&1; then
    cmp --silent "$path" "$existing" || { echo "draft transport asset $name already contains different bytes" >&2; exit 1; }
  else
    gh release upload "$tag" "$path"
  fi
}

while IFS= read -r name; do
  role="$(jq -er --arg name "$name" '.files[] | select(.name == $name) | .role' "$publication_input")"
  if [[ "$role" == "candidate" ]]; then upload_exact "$candidate/$name"; else upload_exact "$evidence"; fi
done < <(jq -r '.files[].name' "$publication_input")
# Upload the transport manifest last; its presence signals a complete draft input.
upload_exact "$publication_input"
echo "Draft release $tag contains a complete verified local publication input."

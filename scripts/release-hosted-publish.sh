#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 3 ]] || { echo "usage: scripts/release-hosted-publish.sh {publish|expose} VERSION INPUT_DIR" >&2; exit 2; }
mode="$1"
version="$2"
input="$3"
tag="v${version}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
publication_input="$input/secondbox-${version}-publication-input.json"

[[ "$mode" == "publish" || "$mode" == "expose" ]] || { echo "unknown hosted publication mode $mode" >&2; exit 2; }
source_commit="$(jq -er '.sourceCommit' "$publication_input")"
test "$(git -C "$repo_root" rev-parse HEAD)" = "$source_commit"
test "$(git -C "$repo_root" rev-parse "refs/tags/${tag}^{commit}")" = "$source_commit"

release_state="$(gh release view "$tag" --json isDraft,isPrerelease)"
missing_transport=false
while IFS= read -r name; do
  [[ -e "$input/$name" ]] || missing_transport=true
done < <(jq -r '.files[].name' "$publication_input")

cleanup_only=false
if $missing_transport; then
  jq -e '.isDraft == false and .isPrerelease == true' <<<"$release_state" >/dev/null || {
    echo "draft publication transport is incomplete" >&2
    exit 1
  }
  if [[ "$mode" == "publish" ]]; then
    echo "Candidate $tag is already public and verified transport cleanup is pending."
    exit 0
  else
    cleanup_only=true
  fi
fi

if ! $cleanup_only; then
  go -C "$repo_root" run ./cmd/secondbox-release-tool verify-publication-input "$input"
fi

if $cleanup_only; then
  while IFS= read -r name; do
    role="$(jq -er --arg name "$name" '.files[] | select(.name == $name) | .role' "$publication_input")"
    if [[ -e "$input/$name" && ( "$role" == "candidate-evidence" || "$name" == *.oci.tar || "$name" == candidate-allowlist.json ) ]]; then
      gh release delete-asset "$tag" "$name" --yes
    fi
  done < <(jq -r '.files[].name' "$publication_input")
  gh release delete-asset "$tag" "${publication_input##*/}" --yes
  echo "Candidate $tag transport cleanup completed after verified public exposure."
  exit 0
fi

work="$(mktemp -d "$repo_root/.tmp/release-hosted.XXXXXX")"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT
candidate="$work/candidate"
mkdir "$candidate"
while IFS= read -r name; do
  cp "$input/$name" "$candidate/$name"
done < <(jq -r '.files[] | select(.role == "candidate") | .name' "$publication_input")
evidence_name="$(jq -er '.files[] | select(.role == "candidate-evidence") | .name' "$publication_input")"
evidence="$input/$evidence_name"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$candidate"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify-candidate-evidence "$candidate/secondbox-${version}-artifact-manifest.json" "$evidence"

if [[ "$mode" == "publish" ]]; then
  export GITHUB_REF_NAME="$tag"
  export GITHUB_SHA="$source_commit"
  "$repo_root/scripts/release-publish-candidate.sh" --defer-release "$version" "$candidate" "$evidence"
  exit 0
fi

gh release edit "$tag" --draft=false --prerelease
"$repo_root/scripts/release-verify-public-candidate.sh" "$version" "$candidate"
while IFS= read -r name; do
  role="$(jq -er --arg name "$name" '.files[] | select(.name == $name) | .role' "$publication_input")"
  if [[ "$role" == "candidate-evidence" || "$name" == *.oci.tar || "$name" == candidate-allowlist.json ]]; then
    gh release delete-asset "$tag" "$name" --yes
  fi
done < <(jq -r '.files[].name' "$publication_input")
gh release delete-asset "$tag" "${publication_input##*/}" --yes
echo "Candidate $tag is public, verified, incomplete, and free of private transport assets."

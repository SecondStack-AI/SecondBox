#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 2 ]] || { echo "usage: scripts/release-local-finalize.sh VERSION QUALIFICATION_FILE" >&2; exit 2; }
version="$1"
qualification="$(realpath "$2")"
tag="v${version}"
repo="SecondStack-AI/SecondBox"
base="https://github.com/${repo}/releases/download/${tag}"

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || { echo "local finalization requires canonical SemVer" >&2; exit 1; }
[[ -f "$qualification" && "${qualification##*/}" == "secondbox-${version}-qualification-attestation.json" ]] || { echo "local finalization requires the canonical qualification file" >&2; exit 1; }
gh auth status >/dev/null
[[ "$(gh repo view "$repo" --json nameWithOwner --jq .nameWithOwner)" == "$repo" ]] || { echo "local finalization cannot resolve the release repository" >&2; exit 1; }
release_state="$(gh release view "$tag" --repo "$repo" --json isDraft,isPrerelease)"
jq -e '.isDraft == false' <<<"$release_state" >/dev/null || { echo "local finalization requires a public candidate" >&2; exit 1; }
npm whoami >/dev/null
test "$(npm view "@secondstack-ai/secondbox@${version}" version --json | jq -r .)" = "$version"
test "$(npm view "@secondstack-ai/secondbox" dist-tags.candidate --json | jq -r .)" = "$version"

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT
curl --fail --location --silent --show-error "$base/SHA256SUMS" --output "$work/SHA256SUMS"
curl --fail --location --silent --show-error "$base/secondbox-${version}-artifact-manifest.json" --output "$work/artifact-manifest.json"
curl --fail --location --silent --show-error "$base/secondbox-deploy_${version}_linux_amd64" --output "$work/secondbox-deploy"
expected="$(awk -v name="secondbox-deploy_${version}_linux_amd64" '$2 == name {print $1}' "$work/SHA256SUMS")"
[[ -n "$expected" ]] || { echo "public candidate checksums omit secondbox-deploy" >&2; exit 1; }
echo "$expected  $work/secondbox-deploy" | sha256sum --check --strict >/dev/null
chmod 0755 "$work/secondbox-deploy"
index="$work/secondbox-${version}-release-index.json"
"$work/secondbox-deploy" release-index --manifest "$work/artifact-manifest.json" --qualification "$qualification" --output "$index"

cache="$work/existing"
mkdir "$cache"
publish_exact() {
  local path="$1"
  local name="${path##*/}"
  local existing="$cache/$name"
  if gh release download "$tag" --repo "$repo" --pattern "$name" --output "$existing" >/dev/null 2>&1; then
    cmp --silent "$path" "$existing" || { echo "final release asset $name already contains different bytes" >&2; exit 1; }
  else
    gh release upload "$tag" "$path" --repo "$repo"
  fi
}

publish_exact "$qualification"
curl --fail --location --silent --show-error "$base/${qualification##*/}" | cmp --silent - "$qualification"
publish_exact "$index"
curl --fail --location --silent --show-error "$base/${index##*/}" | cmp --silent - "$index"
"$work/secondbox-deploy" verify release-index "$base/${index##*/}"
if jq -e '.isPrerelease == true' <<<"$release_state" >/dev/null; then
  gh release edit "$tag" --repo "$repo" --prerelease=false
fi
if [[ "$(npm view "@secondstack-ai/secondbox" dist-tags.latest --json 2>/dev/null | jq -r 'select(type == "string")')" != "$version" ]]; then
  npm dist-tag add "@secondstack-ai/secondbox@${version}" latest
fi
echo "SecondBox $tag is finalized and its public release index verifies."

#!/usr/bin/env bash
set -euo pipefail

dry_run=false
if [[ "${1:-}" == "--dry-run" ]]; then
  dry_run=true
  shift
fi
[[ "$#" -eq 3 ]] || { echo "usage: scripts/release-publish-candidate.sh [--dry-run] VERSION STAGING_DIR CANDIDATE_EVIDENCE" >&2; exit 2; }
version="$1"
stage="$2"
evidence="$3"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tag="v${version}"
manifest="$stage/secondbox-${version}-artifact-manifest.json"

go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify-candidate-evidence "$manifest" "$evidence"
if $dry_run; then
  jq -n --arg version "$version" --arg manifest "sha256:$(sha256sum "$manifest" | awk '{print $1}')" '{mode:"dry-run",version:$version,artifactManifestDigest:$manifest,publicationState:"incomplete-public-candidate"}'
  exit 0
fi

[[ "${GITHUB_REF_NAME:-}" == "$tag" && "${GITHUB_SHA:-}" == "$(jq -er '.sourceCommit' "$manifest")" ]] || {
  echo "candidate publication is not bound to the exact release tag" >&2
  exit 1
}
[[ ! -e "$stage/secondbox-${version}-qualification-attestation.json" && ! -e "$stage/secondbox-${version}-release-index.json" ]] || {
  echo "candidate publication must not include final qualification or release-index authority" >&2
  exit 1
}
publish_image() {
  local name="$1"
  local repository="$2"
  local archive="$stage/${name}.oci.tar"
  local expected
  case "$name" in
    control-plane) expected="$(jq -er '.controlPlane.reference' "$manifest")" ;;
    runner) expected="$(jq -er '.runner.reference' "$manifest")" ;;
    microvm-artifacts) expected="$(jq -er '.microvm.imageReference' "$manifest")" ;;
    *) echo "unknown OCI release object: $name" >&2; exit 1 ;;
  esac
  expected="${expected##*@}"
  current="$(skopeo inspect --format '{{.Digest}}' "docker://${repository}:${tag}" 2>/dev/null || true)"
  if [[ -n "$current" && "$current" != "$expected" ]]; then
    echo "immutable OCI coordinate ${repository}:${tag} already contains different content" >&2
    exit 1
  fi
  if [[ -z "$current" ]]; then
    skopeo copy --all "oci-archive:${archive}" "docker://${repository}:${tag}"
  fi
  test "$(skopeo inspect --format '{{.Digest}}' "docker://${repository}:${tag}")" = "$expected"
}

printf '%s' "$GH_TOKEN" | skopeo login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
publish_image control-plane ghcr.io/secondstack-ai/secondbox/control-plane
publish_image runner ghcr.io/secondstack-ai/secondbox/runner
publish_image microvm-artifacts ghcr.io/secondstack-ai/secondbox/microvm-artifacts

typescript="$stage/secondstack-ai-secondbox-${version}.tgz"
local_integrity="sha512-$(openssl dgst -sha512 -binary "$typescript" | openssl base64 -A)"
published_integrity="$(npm view "@secondstack-ai/secondbox@${version}" dist.integrity --json 2>/dev/null | jq -r 'select(type == "string")' || true)"
if [[ -n "$published_integrity" && "$published_integrity" != "$local_integrity" ]]; then
  echo "immutable npm version already contains different content" >&2
  exit 1
fi
if [[ -z "$published_integrity" ]]; then
  npm publish "$typescript" --access public --tag candidate --provenance
fi
test "$(npm view "@secondstack-ai/secondbox@${version}" dist.integrity --json | jq -r .)" = "$local_integrity"

if ! gh release view "$tag" --json isDraft,isPrerelease >/dev/null 2>&1; then
  gh release create "$tag" --draft --verify-tag --title "SecondBox ${tag}" --notes "Incomplete public release candidate. It is not release authority until the final release index is published."
fi
state="$(gh release view "$tag" --json isDraft,isPrerelease)"
jq -e '.isDraft == true or .isPrerelease == true' <<<"$state" >/dev/null || { echo "existing GitHub release is already final" >&2; exit 1; }

asset_cache="$(mktemp -d)"
cleanup() { rm -rf "$asset_cache"; }
trap cleanup EXIT
for asset in "$stage"/*; do
  [[ "$asset" == *.oci.tar || "${asset##*/}" == candidate-allowlist.json ]] && continue
  name="${asset##*/}"
  if gh release download "$tag" --pattern "$name" --dir "$asset_cache" >/dev/null 2>&1; then
    cmp --silent "$asset" "$asset_cache/$name" || { echo "GitHub release asset $name already contains different content" >&2; exit 1; }
  else
    gh release upload "$tag" "$asset"
  fi
done
gh release edit "$tag" --draft=false --prerelease

resolved=false
for _ in 1 2 3 4 5; do
  if GONOSUMDB= GOPRIVATE= GOPROXY=https://proxy.golang.org go list -m "github.com/SecondStack-AI/SecondBox@${tag}" >/dev/null 2>&1; then
    resolved=true
    break
  fi
  sleep 5
done
$resolved || { echo "public Go module did not resolve at ${tag}" >&2; exit 1; }
curl --fail --location --silent --show-error "https://registry.npmjs.org/@secondstack-ai%2fsecondbox/${version}" >/dev/null
skopeo logout ghcr.io >/dev/null
test "$(skopeo inspect --format '{{.Digest}}' "docker://ghcr.io/secondstack-ai/secondbox/control-plane:${tag}")" = "$(jq -er '.controlPlane.reference | split("@")[-1]' "$manifest")"
test "$(skopeo inspect --format '{{.Digest}}' "docker://ghcr.io/secondstack-ai/secondbox/runner:${tag}")" = "$(jq -er '.runner.reference | split("@")[-1]' "$manifest")"
test "$(skopeo inspect --format '{{.Digest}}' "docker://ghcr.io/secondstack-ai/secondbox/microvm-artifacts:${tag}")" = "$(jq -er '.microvm.imageReference | split("@")[-1]' "$manifest")"
for asset in "$stage"/*; do
  [[ "$asset" == *.oci.tar || "${asset##*/}" == candidate-allowlist.json ]] && continue
  name="${asset##*/}"
  curl --fail --location --silent --show-error "https://github.com/SecondStack-AI/SecondBox/releases/download/${tag}/${name}" | cmp --silent - "$asset" || {
    echo "anonymous GitHub asset verification failed for $name" >&2
    exit 1
  }
done

echo "Public candidate ${tag} is readable but intentionally incomplete."

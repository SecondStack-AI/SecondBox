#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 2 ]] || { echo "usage: scripts/release-verify-public-candidate.sh VERSION STAGING_DIR" >&2; exit 2; }
version="$1"
stage="$2"
tag="v${version}"
manifest="$stage/secondbox-${version}-artifact-manifest.json"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage"
resolved=false
for _ in 1 2 3 4 5 6; do
  if GONOSUMDB= GOPRIVATE= GOPROXY=https://proxy.golang.org go list -m "github.com/SecondStack-AI/SecondBox@${tag}" >/dev/null 2>&1; then
    resolved=true
    break
  fi
  sleep 10
done
$resolved || { echo "public Go module did not resolve at ${tag}" >&2; exit 1; }
test "$(npm view "@secondstack-ai/secondbox@${version}" dist.integrity --json | jq -r .)" = "sha512-$(openssl dgst -sha512 -binary "$stage/secondstack-ai-secondbox-${version}.tgz" | openssl base64 -A)"
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
echo "Public candidate $tag matches the locally prepared publication input."

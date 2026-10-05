#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 2 ]] || { echo "usage: scripts/release-publish.sh VERSION INPUT_DIR" >&2; exit 2; }
version="$1"
input="$2"
tag="v${version}"
manifest="$input/secondbox-${version}-artifact-manifest.json"
arm64_manifest="$input/secondbox-${version}-arm64-artifact-manifest.json"

[[ -f "$manifest" && -f "$arm64_manifest" ]] || { echo "release input does not contain both the amd64 and the arm64 artifact manifest" >&2; exit 1; }
jq -e '.candidate != true' "$manifest" "$arm64_manifest" >/dev/null || { echo "release input is an installer candidate, not a publishable final release" >&2; exit 1; }
jq -e --slurpfile amd64 "$manifest" '.sourceCommit == $amd64[0].sourceCommit' "$arm64_manifest" >/dev/null || { echo "the amd64 and arm64 artifact sets were staged from different commits" >&2; exit 1; }

# The arm64 artifact set publishes its images under the -arm64 tag suffix.
printf '%s' "$GH_TOKEN" | skopeo login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
for image in control-plane runner installer-tools microvm-artifacts runner-gvisor gvisor-artifacts; do
  skopeo copy --all "oci-archive:$input/$image.oci.tar" "docker://ghcr.io/secondstack-ai/secondbox/$image:$tag"
done
for image in control-plane runner installer-tools microvm-artifacts; do
  skopeo copy --all "oci-archive:$input/$image-arm64.oci.tar" "docker://ghcr.io/secondstack-ai/secondbox/$image:$tag-arm64"
done
skopeo logout ghcr.io >/dev/null

if ! npm view "@secondstack-ai/secondbox@${version}" version >/dev/null 2>&1; then
  npm publish "$input/secondstack-ai-secondbox-${version}.tgz" --access public --tag latest --provenance
fi

for name in control-plane.oci.tar runner.oci.tar installer-tools.oci.tar microvm-artifacts.oci.tar runner-gvisor.oci.tar gvisor-artifacts.oci.tar candidate-allowlist.json \
  control-plane-arm64.oci.tar runner-arm64.oci.tar installer-tools-arm64.oci.tar microvm-artifacts-arm64.oci.tar candidate-allowlist-arm64.json; do
  gh release delete-asset "$tag" "$name" --yes
done

gh release edit "$tag" \
  --draft=false \
  --prerelease=false \
  --latest \
  --title "SecondBox $tag"

echo "Published stable release $tag."

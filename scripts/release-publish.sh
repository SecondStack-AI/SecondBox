#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 2 ]] || { echo "usage: scripts/release-publish.sh VERSION INPUT_DIR" >&2; exit 2; }
version="$1"
input="$2"
tag="v${version}"
manifest="$input/secondbox-${version}-artifact-manifest.json"

[[ -f "$manifest" ]] || { echo "release input does not contain the artifact manifest" >&2; exit 1; }
jq -e '.candidate != true' "$manifest" >/dev/null || { echo "release input is an installer candidate, not a publishable final release" >&2; exit 1; }

printf '%s' "$GH_TOKEN" | skopeo login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
skopeo copy --all "oci-archive:$input/control-plane.oci.tar" "docker://ghcr.io/secondstack-ai/secondbox/control-plane:$tag"
skopeo copy --all "oci-archive:$input/runner.oci.tar" "docker://ghcr.io/secondstack-ai/secondbox/runner:$tag"
skopeo copy --all "oci-archive:$input/installer-tools.oci.tar" "docker://ghcr.io/secondstack-ai/secondbox/installer-tools:$tag"
skopeo copy --all "oci-archive:$input/microvm-artifacts.oci.tar" "docker://ghcr.io/secondstack-ai/secondbox/microvm-artifacts:$tag"
skopeo logout ghcr.io >/dev/null

if ! npm view "@secondstack-ai/secondbox@${version}" version >/dev/null 2>&1; then
  npm publish "$input/secondstack-ai-secondbox-${version}.tgz" --access public --tag latest --provenance
fi

for name in control-plane.oci.tar runner.oci.tar installer-tools.oci.tar microvm-artifacts.oci.tar candidate-allowlist.json; do
  gh release delete-asset "$tag" "$name" --yes
done

gh release edit "$tag" \
  --draft=false \
  --prerelease=false \
  --latest \
  --title "SecondBox $tag" \
  --notes "Guided Linux amd64 install: curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh | sh. SDK: npm install @secondstack-ai/secondbox@${version}"

echo "Published stable release $tag."

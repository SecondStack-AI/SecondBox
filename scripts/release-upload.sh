#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 2 ]] || { echo "usage: scripts/release-upload.sh VERSION OUTPUT_DIR" >&2; exit 2; }
version="$1"
output="$2"
tag="v${version}"

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || {
  echo "release upload requires canonical SemVer without a v prefix" >&2
  exit 1
}
[[ -d "$output" ]] || { echo "release output directory does not exist: $output" >&2; exit 1; }
[[ -f "$output/secondbox-${version}-artifact-manifest.json" ]] || {
  echo "release output does not contain the v${version} artifact manifest" >&2
  exit 1
}

gh auth status >/dev/null
if gh release view "$tag" --json isDraft >/dev/null 2>&1; then
  test "$(gh release view "$tag" --json isDraft --jq .isDraft)" = true || {
    echo "release $tag is already public" >&2
    exit 1
  }
else
  gh release create "$tag" --draft --verify-tag --title "SecondBox $tag" --notes "Publishing locally built artifacts."
fi

gh release upload "$tag" "$output"/* --clobber
gh workflow run release.yml --ref main -f version="$version"
echo "Uploaded $tag and dispatched the GitHub publisher."

#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -ge 2 && "$#" -le 3 ]] || { echo "usage: scripts/release-upload.sh VERSION OUTPUT_DIR [NOTES_FILE]" >&2; exit 2; }
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

notes="$(mktemp)"
trap 'rm -f "$notes"' EXIT
if [[ -n "${3:-}" ]]; then
  [[ -f "$3" && -r "$3" ]] || { echo "release notes file is not readable: $3" >&2; exit 1; }
  cat -- "$3" >"$notes"
else
  # Read the immutable tag, never unrelated notes in the caller's checkout.
  git rev-parse --verify "refs/tags/$tag^{commit}" >/dev/null
  notes_path="docs/releases/$tag.md"
  if git cat-file -e "refs/tags/$tag:$notes_path" 2>/dev/null; then
    git show "refs/tags/$tag:$notes_path" >"$notes"
  else
    printf 'Publishing locally built artifacts.\n' >"$notes"
  fi
fi
cat >>"$notes" <<FOOTER

## Install

Guided Linux amd64 install:

\`\`\`sh
curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/download/$tag/install.sh | sh
\`\`\`

SDKs: \`npm install @secondstack-ai/secondbox@$version\` and \`go get github.com/SecondStack-AI/SecondBox@$tag\`
FOOTER

gh auth status >/dev/null
if gh release view "$tag" --json isDraft >/dev/null 2>&1; then
  test "$(gh release view "$tag" --json isDraft --jq .isDraft)" = true || {
    echo "release $tag is already public" >&2
    exit 1
  }
  gh release edit "$tag" --notes-file "$notes"
else
  gh release create "$tag" --draft --verify-tag --title "SecondBox $tag" --notes-file "$notes"
fi

gh release upload "$tag" "$output"/* --clobber
gh workflow run release.yml --ref main -f version="$version"
echo "Uploaded $tag and dispatched the GitHub publisher."

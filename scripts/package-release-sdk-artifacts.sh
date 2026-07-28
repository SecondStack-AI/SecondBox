#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 3 ]]; then
  echo "Usage: scripts/package-release-sdk-artifacts.sh OUTPUT_DIRECTORY RELEASE_VERSION SOURCE_COMMIT" >&2
  exit 2
fi

output_directory="$(realpath -e -- "$1")"
release_version="$2"
source_commit="$3"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -L "$1" || ! -d "$output_directory" ]]; then
  echo "SecondBox SDK packaging requires an existing non-symbolic-link output directory" >&2
  exit 1
fi
if [[ ! "$release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
  echo "SecondBox SDK release version must be a SemVer value without a leading v: $release_version" >&2
  exit 1
fi
if [[ ! "$source_commit" =~ ^[0-9a-f]{40}$ ]] ||
   [[ "$(git -C "$repo_root" rev-parse HEAD)" != "$source_commit" ]]; then
  echo "SecondBox SDK packaging source commit must equal the checked-out commit" >&2
  exit 1
fi
if [[ -n "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" ]]; then
  echo "SecondBox SDK packaging requires a clean source tree" >&2
  exit 1
fi
for required_command in git gzip jq node npm sha256sum tar; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox SDK packaging requires command: $required_command" >&2
    exit 1
  fi
done

go_package_name="secondbox-$release_version-go-sdk"
go_package_path="$output_directory/$go_package_name.tar.gz"
typescript_package_name="secondstack-ai-secondbox-$release_version.tgz"
typescript_package_path="$output_directory/$typescript_package_name"
checksum_path="$output_directory/secondbox-$release_version-sdk.SHA256SUMS"
for output_path in "$go_package_path" "$typescript_package_path" "$checksum_path"; do
  if [[ -e "$output_path" ]]; then
    echo "SecondBox SDK packaging refuses to overwrite: $output_path" >&2
    exit 1
  fi
done

working_directory="$(mktemp -d)"
cleanup_release_sdk_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox SDK packaging failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_sdk_working_directory EXIT

git -C "$repo_root" archive \
  --format=tar \
  --prefix="$go_package_name/" \
  "$source_commit" \
  LICENSE \
  go.mod \
  go.sum \
  sdk/go |
  gzip --no-name >"$go_package_path"

typescript_source="$working_directory/typescript-source"
install -d -m 0755 "$typescript_source"
git -C "$repo_root" archive --format=tar "$source_commit" sdk/typescript |
  tar -xf - -C "$working_directory"
jq --arg version "$release_version" \
  '.version = $version' \
  "$typescript_source/package.json" \
  >"$working_directory/package.json"
install -m 0644 "$working_directory/package.json" "$typescript_source/package.json"
PATH="$repo_root/node_modules/.bin:$PATH" node "$typescript_source/prepare-package.mjs"
npm pack \
  --ignore-scripts \
  --json \
  --pack-destination "$output_directory" \
  "$typescript_source" >"$working_directory/npm-pack-result.json"
packed_name="$(jq -er '.[0].filename' "$working_directory/npm-pack-result.json")"
if [[ "$packed_name" != "$typescript_package_name" ||
      ! -f "$typescript_package_path" ||
      -L "$typescript_package_path" ]]; then
  echo "SecondBox TypeScript SDK package name is not the exact release subject: $packed_name" >&2
  exit 1
fi
if [[ "$(tar -xOf "$typescript_package_path" package/package.json | jq -r '.version')" != "$release_version" ]]; then
  echo "SecondBox TypeScript SDK package contains the wrong release version" >&2
  exit 1
fi

(
  cd "$output_directory"
  sha256sum \
    "$(basename "$go_package_path")" \
    "$(basename "$typescript_package_path")" \
    >"$(basename "$checksum_path")"
)

cleanup_release_sdk_working_directory
trap - EXIT
printf '%s\n%s\n' "$go_package_path" "$typescript_package_path"

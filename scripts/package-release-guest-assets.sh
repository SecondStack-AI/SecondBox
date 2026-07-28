#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 4 ]]; then
  echo "Usage: scripts/package-release-guest-assets.sh ARTIFACT_DIRECTORY OUTPUT_DIRECTORY RELEASE_VERSION SOURCE_COMMIT" >&2
  exit 2
fi

artifact_directory="$(realpath -e -- "$1")"
output_directory="$(realpath -e -- "$2")"
release_version="$3"
source_commit="$4"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
trusted_public_key="${SECONDBOX_RELEASE_GUEST_TRUSTED_PUBLIC_KEY:?set SECONDBOX_RELEASE_GUEST_TRUSTED_PUBLIC_KEY}"
trusted_public_key_sha256="${SECONDBOX_RELEASE_GUEST_TRUSTED_PUBLIC_KEY_SHA256:?set SECONDBOX_RELEASE_GUEST_TRUSTED_PUBLIC_KEY_SHA256}"

if [[ -L "$1" || ! -d "$artifact_directory" ||
      -L "$2" || ! -d "$output_directory" ]]; then
  echo "SecondBox guest packaging requires regular artifact and output directories" >&2
  exit 1
fi
if [[ ! "$release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
  echo "SecondBox guest release version must be a SemVer value without a leading v: $release_version" >&2
  exit 1
fi
if [[ ! "$source_commit" =~ ^[0-9a-f]{40}$ ]] ||
   [[ "$(git -C "$repo_root" rev-parse HEAD)" != "$source_commit" ]]; then
  echo "SecondBox guest packaging source commit must equal the checked-out commit" >&2
  exit 1
fi
if [[ -n "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" ]]; then
  echo "SecondBox guest packaging requires a clean source tree" >&2
  exit 1
fi
if [[ -L "$trusted_public_key" || ! -f "$trusted_public_key" ]]; then
  echo "SecondBox guest packaging requires a regular independently provisioned trusted public key" >&2
  exit 1
fi
if [[ ! "$trusted_public_key_sha256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "SECONDBOX_RELEASE_GUEST_TRUSTED_PUBLIC_KEY_SHA256 must be 64 lowercase hexadecimal characters" >&2
  exit 1
fi
for required_command in git jq openssl sha256sum tar; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox guest packaging requires command: $required_command" >&2
    exit 1
  fi
done

"$repo_root/runner/scripts/microvm-image/verify.sh" \
  "$artifact_directory" \
  "$trusted_public_key" \
  "$trusted_public_key_sha256"

if [[ "$(jq -r '.artifactVersion // empty' "$artifact_directory/manifest.json")" != "$release_version" ]]; then
  echo "SecondBox guest artifact version does not match the release version" >&2
  exit 1
fi
if [[ "$(jq -r '.architecture // empty' "$artifact_directory/manifest.json")" != "amd64" ]]; then
  echo "SecondBox guest artifact architecture is not amd64" >&2
  exit 1
fi
if [[ "$(jq -r '.builder.gitCommit // empty' "$artifact_directory/kernel-provenance.json")" != "$source_commit" ||
      "$(jq -r '.inputs.gitCommit // empty' "$artifact_directory/rootfs-source-manifest.json")" != "$source_commit" ]]; then
  echo "SecondBox guest provenance does not identify the release source commit" >&2
  exit 1
fi
bundled_key_fingerprint="$(
  openssl pkey -pubin -in "$artifact_directory/signing.pub" -outform DER 2>/dev/null |
    sha256sum |
    awk '{print $1}'
)"
if [[ "$bundled_key_fingerprint" != "$trusted_public_key_sha256" ]]; then
  echo "SecondBox guest bundled verification key does not match the approved trust anchor" >&2
  exit 1
fi

package_name="secondbox-$release_version-guest-amd64"
package_archive="$output_directory/$package_name.tar.gz"
package_checksums="$output_directory/$package_name.SHA256SUMS"
for output_path in "$package_archive" "$package_checksums"; do
  if [[ -e "$output_path" ]]; then
    echo "SecondBox guest packaging refuses to overwrite: $output_path" >&2
    exit 1
  fi
done
if find "$artifact_directory" -type l -print -quit | grep -q .; then
  echo "SecondBox guest package must not contain symbolic links" >&2
  exit 1
fi

source_epoch="$(git -C "$repo_root" show -s --format=%ct "$source_commit")"
tar \
  --sort=name \
  --mtime="@$source_epoch" \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  --transform="s|^\\./|$package_name/|" \
  -C "$artifact_directory" \
  -czf "$package_archive" \
  .
(
  cd "$output_directory"
  sha256sum "$(basename "$package_archive")" >"$(basename "$package_checksums")"
)
echo "$package_archive"

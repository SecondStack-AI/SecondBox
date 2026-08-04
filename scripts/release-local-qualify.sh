#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 3 ]] || { echo "usage: scripts/release-local-qualify.sh VERSION OPERATOR_MANIFEST WORK_DIR" >&2; exit 2; }
version="$1"
operator_manifest="$2"
work_root="$3"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tag="v${version}"

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || { echo "local qualification requires canonical SemVer" >&2; exit 1; }
operator_manifest="$(realpath "$operator_manifest")"
work_root="$(realpath -m "$work_root")"
[[ -f "$operator_manifest" ]] || { echo "local qualification operator manifest is not a regular file" >&2; exit 1; }
case "$operator_manifest" in "$repo_root"/*) echo "local qualification operator manifest must be outside the source checkout" >&2; exit 1 ;; esac
case "$work_root" in "$repo_root"/*) echo "local qualification work directory must be outside the source checkout" >&2; exit 1 ;; esac
[[ ! -e "$work_root" ]] || { echo "local qualification work directory already exists: $work_root" >&2; exit 1; }
[[ -r /dev/kvm && -w /dev/kvm ]] || { echo "local qualification requires read-write /dev/kvm access" >&2; exit 1; }
: "${SECONDBOX_URL:?local qualification requires SECONDBOX_URL}"
: "${SECONDBOX_TOKEN:?local qualification requires SECONDBOX_TOKEN}"
: "${SECONDBOX_TENANT_REF:?local qualification requires SECONDBOX_TENANT_REF}"
: "${SECONDBOX_SUBJECT_REF:?local qualification requires SECONDBOX_SUBJECT_REF}"

mkdir -m 0700 "$work_root"
artifact_directory="$work_root/artifacts"
mkdir -m 0700 "$artifact_directory"
base="https://github.com/SecondStack-AI/SecondBox/releases/download/${tag}"
checksums="$work_root/SHA256SUMS"
suite="$work_root/secondbox-${version}-source-free-qualify"
curl --fail --location --silent --show-error "$base/SHA256SUMS" --output "$checksums"
curl --fail --location --silent --show-error "$base/secondbox-${version}-source-free-qualify" --output "$suite"
expected="$(awk -v name="secondbox-${version}-source-free-qualify" '$2 == name {print $1}' "$checksums")"
[[ -n "$expected" ]] || { echo "public candidate checksums omit the source-free suite" >&2; exit 1; }
echo "$expected  $suite" | sha256sum --check --strict >/dev/null
chmod 0755 "$suite"

export SECONDBOX_RELEASE_ARTIFACT_MANIFEST_URL="$base/secondbox-${version}-artifact-manifest.json"
export SECONDBOX_SOURCE_FREE_OPERATOR_MANIFEST="$operator_manifest"
export SECONDBOX_SOURCE_FREE_ROOT="$work_root/source-free"
export SECONDBOX_SOURCE_FREE_ARTIFACT_DIRECTORY="$artifact_directory"
export SECONDBOX_SOURCE_FREE_QUALIFICATION_OUTPUT="$work_root/secondbox-${version}-qualification-attestation.json"
(
  cd "$work_root"
  "$suite"
)
test -s "$SECONDBOX_SOURCE_FREE_QUALIFICATION_OUTPUT"
echo "$SECONDBOX_SOURCE_FREE_QUALIFICATION_OUTPUT"

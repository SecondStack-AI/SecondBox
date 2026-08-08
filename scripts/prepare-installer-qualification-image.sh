#!/usr/bin/env bash
set -euo pipefail

readonly image_url='https://cloud-images.ubuntu.com/releases/noble/release-20260725/ubuntu-24.04-server-cloudimg-amd64.img'
readonly image_sha256='d1940f7d69d343355e183dff1e08a59852d32e7309baa7a4bad8365b11b005ac'

if [[ "$#" -eq 1 && "$1" == --print-sha256 ]]; then
  printf '%s\n' "$image_sha256"
  exit 0
fi

if [[ "$#" -ne 1 ]]; then
  echo 'usage: scripts/prepare-installer-qualification-image.sh OUTPUT_IMAGE | --print-sha256' >&2
  exit 2
fi

output="$1"
[[ "$output" == /* && "$(dirname "$output")" != / ]] || {
  echo 'installer qualification image output must be an absolute non-root path' >&2
  exit 1
}
if [[ -e "$output" || -L "$output" ]]; then
  [[ -f "$output" && ! -L "$output" ]] || {
    echo "installer qualification image output is not a regular non-symlink file: $output" >&2
    exit 1
  }
  printf '%s  %s\n' "$image_sha256" "$output" | sha256sum --check --status || {
    echo 'existing installer qualification image digest differs from the pinned Ubuntu image' >&2
    exit 1
  }
  printf '%s\n' "$image_sha256"
  exit 0
fi

mkdir -p "$(dirname "$output")"
temporary="$(mktemp "$(dirname "$output")/.secondbox-qualification-image.XXXXXX")"
trap 'rm -f -- "$temporary"' EXIT
curl --fail --location --proto '=https' --tlsv1.2 --output "$temporary" "$image_url"
printf '%s  %s\n' "$image_sha256" "$temporary" | sha256sum --check --status || {
  echo 'installer qualification image digest differs from the pinned Ubuntu image' >&2
  exit 1
}
chmod 0644 "$temporary"
mv -- "$temporary" "$output"
trap - EXIT
printf '%s\n' "$image_sha256"

#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 3 ]] || {
  echo "usage: scripts/generate-install-bootstrap.sh VERSION BINARY_SHA256 OUTPUT" >&2
  exit 2
}
version="$1"
binary_sha256="$2"
output="$3"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || {
  echo "install bootstrap version must be canonical SemVer without a v prefix" >&2
  exit 1
}
[[ "$binary_sha256" =~ ^[0-9a-f]{64}$ ]] || {
  echo "install bootstrap binary digest must be lowercase SHA-256" >&2
  exit 1
}
[[ ! -e "$output" && ! -L "$output" ]] || {
  echo "install bootstrap output already exists: $output" >&2
  exit 1
}

sed \
  -e "s/@VERSION@/$version/g" \
  -e "s/@BINARY_SHA256@/$binary_sha256/g" \
  "$repo_root/scripts/install-bootstrap.sh.in" >"$output"
chmod 0644 "$output"
sh -n "$output"

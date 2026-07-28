#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "Usage: scripts/install-protoc.sh TARGET_DIRECTORY" >&2
  exit 64
fi

target_directory="$1"
version="35.1"
archive_name="protoc-${version}-linux-x86_64.zip"
archive_sha256="6930ebf62bd4ea607b98fff052596c6ee564b9835b4ce172c75a3f53ae9d91b7"
archive_url="https://github.com/protocolbuffers/protobuf/releases/download/v${version}/${archive_name}"

work_directory="$(mktemp -d)"
cleanup() {
  rm -rf -- "$work_directory"
}
trap cleanup EXIT

curl \
  --fail \
  --location \
  --proto '=https' \
  --tlsv1.2 \
  --output "$work_directory/$archive_name" \
  "$archive_url"

printf '%s  %s\n' \
  "$archive_sha256" \
  "$work_directory/$archive_name" |
  sha256sum --check --strict

mkdir -p "$target_directory"
unzip -q "$work_directory/$archive_name" -d "$target_directory"

actual_version="$("$target_directory/bin/protoc" --version)"
if [[ "$actual_version" != "libprotoc $version" ]]; then
  echo "SecondBox protoc version = $actual_version, want libprotoc $version" >&2
  exit 1
fi

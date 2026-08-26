#!/usr/bin/env bash
set -euo pipefail

# Fetches the pinned gVisor runsc release for the Task 0H probe and verifies
# its published SHA-512 before installing it into the requested directory.
# The pin may advance only as a reviewed dependency change.

readonly RUNSC_RELEASE="20260817.0"
readonly RUNSC_ARCH="x86_64"
readonly RUNSC_SHA512="84936438d583ec976800f464e75a83e1515f0890b451b9b4db219c4472b54ca9b106a6772ee683f1e64cce2128871d7637b14d800591f8451b8137f6c39fb2ef"
readonly RUNSC_URL="https://storage.googleapis.com/gvisor/releases/release/${RUNSC_RELEASE}/${RUNSC_ARCH}/runsc"

usage() {
  echo "usage: $0 --output <directory>" >&2
}

output_dir=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --output)
      [[ $# -ge 2 ]] || { usage; exit 1; }
      output_dir="$2"
      shift 2
      ;;
    *)
      usage
      exit 1
      ;;
  esac
done

[[ -n "$output_dir" ]] || { usage; exit 1; }
[[ "$(uname -m)" == "$RUNSC_ARCH" ]] || {
  echo "SecondBox runsc pin covers ${RUNSC_ARCH} only; host is $(uname -m)" >&2
  exit 1
}

mkdir -p -- "$output_dir"
destination="$output_dir/runsc"

checksum_of() {
  sha512sum -- "$1" | awk '{ print $1 }'
}

if [[ -f "$destination" && "$(checksum_of "$destination")" == "$RUNSC_SHA512" ]]; then
  echo "SecondBox pinned runsc already present: $destination"
else
  staging="$(mktemp -- "$output_dir/.runsc.XXXXXXXX")"
  trap 'rm -f -- "$staging"' EXIT
  curl --fail --silent --show-error --location --max-time 300 --output "$staging" -- "$RUNSC_URL"
  actual="$(checksum_of "$staging")"
  [[ "$actual" == "$RUNSC_SHA512" ]] || {
    echo "SecondBox runsc download does not match the pinned SHA-512" >&2
    echo "expected ${RUNSC_SHA512}" >&2
    echo "actual   ${actual}" >&2
    exit 1
  }
  chmod 0755 -- "$staging"
  mv -- "$staging" "$destination"
  trap - EXIT
  echo "SecondBox pinned runsc installed: $destination"
fi

version_output="$("$destination" --version)"
grep -q "release-${RUNSC_RELEASE}" <<<"$version_output" || {
  echo "SecondBox pinned runsc reports an unexpected version:" >&2
  echo "$version_output" >&2
  exit 1
}
echo "$version_output"

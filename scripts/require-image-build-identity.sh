#!/bin/sh
# Refuses an image build whose RELEASE_VERSION or SOURCE_COMMIT build argument
# would leave the binaries reporting the unstamped development identity.
# Dockerfiles bind-mount this file and run it before the slow build steps.
set -eu
release_version="${1-}"
source_commit="${2-}"
semver='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
if [ "$release_version" = 0.0.0-development ] || ! printf '%s\n' "$release_version" | grep -Eqx "$semver"; then
  echo "SecondBox image build identity: RELEASE_VERSION must be an explicit SemVer version without build metadata and other than 0.0.0-development, got '$release_version'" >&2
  exit 1
fi
if ! printf '%s\n' "$source_commit" | grep -Eqx '[0-9a-f]{40}'; then
  echo "SecondBox image build identity: SOURCE_COMMIT must be a full 40-character lowercase hexadecimal Git commit, got '$source_commit'" >&2
  exit 1
fi

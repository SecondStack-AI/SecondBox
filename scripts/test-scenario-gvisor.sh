#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ "$(uname -s)" != "Linux" || "$(uname -m)" != "x86_64" ]]; then
  echo "SecondBox gVisor scenario requires Linux x86_64" >&2
  exit 1
fi
[[ ! -e /dev/kvm ]] || {
  echo "SecondBox gVisor scenario qualifies hosts without /dev/kvm" >&2
  exit 1
}
: "${SECONDBOX_GVISOR_LINUX_BUILD:?set SECONDBOX_GVISOR_LINUX_BUILD to an exact local build directory}"
build_root="$(realpath -e "$SECONDBOX_GVISOR_LINUX_BUILD")"
[[ "$build_root" == "$SECONDBOX_GVISOR_LINUX_BUILD" && ! -L "$build_root" && -d "$build_root" ]] || {
  echo "SECONDBOX_GVISOR_LINUX_BUILD must be a clean absolute non-symlink directory" >&2
  exit 1
}

runsc="$build_root/bin/runsc"
agent="$build_root/bin/secondbox-guest-agent"
flat_root="$build_root/rootfs"
for path in "$runsc" "$agent"; do
  [[ -f "$path" && ! -L "$path" && -x "$path" ]] || {
    echo "SecondBox gVisor local build is missing an executable input: $path" >&2
    exit 1
  }
done
[[ -d "$flat_root" && ! -L "$flat_root" ]] || {
  echo "SecondBox gVisor local flat root is invalid: $flat_root" >&2
  exit 1
}
# The runsc binary must be the reviewed pin; fetch-runsc.sh holds the exact
# SHA-512 and refuses drift.
pinned_sha512="$(sed -n 's/^readonly RUNSC_SHA512="\([a-f0-9]\{128\}\)"$/\1/p' "$repo_root/runner/scripts/fetch-runsc.sh")"
[[ -n "$pinned_sha512" ]] || {
  echo "SecondBox gVisor pin could not be read from fetch-runsc.sh" >&2
  exit 1
}
actual_sha512="$(sha512sum "$runsc" | awk '{print $1}')"
[[ "$actual_sha512" == "$pinned_sha512" ]] || {
  echo "SecondBox gVisor local runsc differs from the reviewed pin" >&2
  exit 1
}

temporary="$(mktemp -d /tmp/secondbox-gvisor-scenario-input.XXXXXX)"
cleanup() {
  status="$?"
  trap - EXIT
  if ! rm -rf -- "$temporary"; then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

digest_text() {
  printf '%s' "$1" | sha256sum | awk '{print "sha256:" $1}'
}
digest_file() {
  sha256sum "$1" | awk '{print "sha256:" $1}'
}
digest_canonical_json() {
  jq --compact-output --join-output . "$1" |
    sha256sum |
    awk '{print "sha256:" $1}'
}

runsc_release="$(sed -n 's/^readonly RUNSC_RELEASE="\([0-9.]*\)"$/\1/p' "$repo_root/runner/scripts/fetch-runsc.sh")"
runtime_digest="$(digest_text "secondbox-gvisor-runtime-release-$runsc_release-linux-amd64")"
toolchain_digest="$(digest_text "secondbox-gvisor-toolchain-release-$runsc_release-linux-amd64")"
source_oci_digest="$(digest_text 'alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce')"
(cd "$repo_root/runner" && go run ./cmd/secondbox-prepare-gvisor-flat-root "$flat_root")
flat_root_digest="$(cd "$repo_root/runner" && go run ./cmd/secondbox-flat-root-digest "$flat_root")"
materialization="$temporary/materialization.json"
jq -cn \
  --arg runtime "$runtime_digest" \
  --arg toolchain "$toolchain_digest" \
  --arg source "$source_oci_digest" \
  --arg flatRoot "$flat_root_digest" \
  --arg agent "$(digest_file "$agent")" \
  --arg runsc "$(digest_file "$runsc")" \
  --arg helperBuild "runsc-release-$runsc_release" \
  '{
    schemaVersion: "secondbox.runner/backend-materialization/v1",
    key: {
      backendKind: "gvisor",
      guestArchitecture: "amd64",
      runtimeManifestDigest: $runtime,
      toolchainManifestDigest: $toolchain
    },
    sourceOciManifestDigest: $source,
    flatRootDigest: $flatRoot,
    launchArtifacts: [
      {id: "guest-agent", sha256: $agent},
      {id: "runsc", sha256: $runsc}
    ],
    agentProtocolGeneration: 1,
    agentFeatures: ["exec-streaming", "file-streaming", "port-proxy", "pty"],
    backendBuildId: "secondbox-gvisor-scenario",
    helperBuildId: $helperBuild
  }' >"$materialization"
materialization_digest="$(digest_canonical_json "$materialization")"

export SECONDBOX_SCENARIO_COMPUTE_BACKEND=gvisor
export SECONDBOX_SCENARIO_GVISOR_BUILD="$build_root"
export SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION="$materialization"
export SECONDBOX_SCENARIO_GVISOR_MATERIALIZATION_DIGEST="$materialization_digest"
export SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST="$runtime_digest"
export SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST="$toolchain_digest"
export SECONDBOX_SCENARIO_ARTIFACT_MANIFEST_DIGEST="$materialization_digest"

"$repo_root/scripts/test-scenario.sh"

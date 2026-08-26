#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

fail() {
  echo "SecondBox Microsandbox macOS scenario prerequisite failed: $*" >&2
  exit 1
}

[[ "$(uname -s)" == "Darwin" && "$(uname -m)" == "arm64" ]] ||
  fail "Apple Silicon macOS is required"
[[ "$(sysctl -n kern.hv_support)" == "1" ]] ||
  fail "Hypervisor.framework is unavailable"
: "${SECONDBOX_MICROSANDBOX_MACOS_BUILD:?set SECONDBOX_MICROSANDBOX_MACOS_BUILD to the exact local build directory}"
build_root="$(cd "$SECONDBOX_MICROSANDBOX_MACOS_BUILD" && pwd -P)"
[[ "$build_root" == "$SECONDBOX_MICROSANDBOX_MACOS_BUILD" && ! -L "$build_root" ]] ||
  fail "SECONDBOX_MICROSANDBOX_MACOS_BUILD must be a clean absolute non-symlink directory"

export PATH="/opt/homebrew/opt/go@1.25/bin:/opt/homebrew/bin:/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
for tool in diskutil docker e2fsck go ipconfig jq mke2fs otool route shasum sysctl tune2fs; do
  command -v "$tool" >/dev/null || fail "missing command: $tool"
done
docker info >/dev/null || fail "the local Docker engine is unavailable"

runner="$build_root/runtime/bin/secondbox-runner"
helper="$build_root/runtime/bin/secondbox-microsandbox-helper"
agentd="$build_root/runtime/bin/agentd"
firmware="$build_root/runtime/lib/libkrunfw.5.dylib"
flat_root="$build_root/rootfs"
evidence="$build_root/build-evidence.txt"
for path in "$runner" "$helper" "$agentd" "$firmware" "$evidence"; do
  [[ -f "$path" && ! -L "$path" ]] || fail "local build input is invalid: $path"
done
[[ -x "$runner" && -x "$helper" && -x "$agentd" && -d "$flat_root" && ! -L "$flat_root" ]] ||
  fail "local runtime bundle is incomplete"
# A local or ad-hoc signature may be mechanically required for the helper to
# execute on this host, but mini1 cannot qualify the repository signing step or
# an operator's production identity. Real Hypervisor.framework boots below are
# the execution gate; production signing remains external evidence.
linked_libraries="$(otool -L "$helper" | awk 'NR > 1 {print $1}')"
if grep -Eq '/Users/|/opt/homebrew|/usr/local|libkrunfw' <<<"$linked_libraries"; then
  fail "helper contains a mutable runtime-library dependency"
fi
grep -Fxq 'upstream_commit=5b335537afad433ad2c0308cb54de13b7015b4e7' "$evidence" ||
  fail "local build has the wrong Microsandbox revision"
grep -Fxq 'patched_tree=daf8457b13e5f124a63e23a12edbd8482d7da43a' "$evidence" ||
  fail "local build has the wrong patched tree"
grep -Fxq 'helper_lock_sha256=72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c' "$evidence" ||
  fail "local build has the wrong helper lock"

temporary="$(mktemp -d /tmp/secondbox-microsandbox-macos-scenario.XXXXXXXX)"
temporary="$(cd "$temporary" && pwd -P)"
cleanup() {
  status="$?"
  trap - EXIT
  rm -rf -- "$temporary" || status=1
  exit "$status"
}
trap cleanup EXIT
workspace_root="$temporary/workspaces"
mkdir -p -- "$workspace_root"
filesystem_device="$(df -P "$workspace_root" | awk 'NR == 2 {print $1}')"
diskutil info "$filesystem_device" | grep -q 'File System Personality:.*APFS' ||
  fail "scenario workspace root is not on APFS"
default_interface="$(route -n get default 2>/dev/null | awk '/interface:/{print $2; exit}')"
[[ -n "$default_interface" ]] || fail "the default host network interface is unavailable"
native_advertised_address="$(ipconfig getifaddr "$default_interface" 2>/dev/null || true)"
[[ "$native_advertised_address" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail "the default host interface lacks an IPv4 address"

digest_text() {
  printf '%s' "$1" | shasum -a 256 | awk '{print "sha256:" $1}'
}
digest_file() {
  shasum -a 256 "$1" | awk '{print "sha256:" $1}'
}
digest_canonical_json() {
  jq --compact-output --join-output . "$1" | shasum -a 256 | awk '{print "sha256:" $1}'
}

runtime_digest="$(digest_text 'secondbox-microsandbox-runtime-v0.6.8-darwin-arm64')"
toolchain_digest="$(digest_text 'secondbox-microsandbox-toolchain-v0.6.8-darwin-arm64')"
source_oci_digest="$(sed -n 's#^rootfs_image=.*@\(sha256:[a-f0-9]\{64\}\)$#\1#p' "$evidence")"
[[ "$source_oci_digest" =~ ^sha256:[a-f0-9]{64}$ ]] ||
  fail "local build lacks a pinned source OCI digest"
flat_root_digest="$(cd "$repo_root/runner" && GOCACHE="$temporary/go-cache" go run ./cmd/secondbox-flat-root-digest "$flat_root")"
materialization="$temporary/materialization.json"
jq -cn \
  --arg runtime "$runtime_digest" \
  --arg toolchain "$toolchain_digest" \
  --arg source "$source_oci_digest" \
  --arg flatRoot "$flat_root_digest" \
  --arg agentd "$(digest_file "$agentd")" \
  --arg helper "$(digest_file "$helper")" \
  --arg firmware "$(digest_file "$firmware")" \
  '{
    schemaVersion: "secondbox.runner/backend-materialization/v1",
    key: {
      backendKind: "microsandbox",
      guestArchitecture: "arm64",
      runtimeManifestDigest: $runtime,
      toolchainManifestDigest: $toolchain
    },
    sourceOciManifestDigest: $source,
    flatRootDigest: $flatRoot,
    launchArtifacts: [
      {id: "agentd", sha256: $agentd},
      {id: "helper", sha256: $helper},
      {id: "libkrunfw", sha256: $firmware}
    ],
    agentProtocolGeneration: 6,
    agentFeatures: ["exec-streaming", "file-streaming", "pty", "tcp"],
    backendBuildId: "microsandbox-0.6.8-msb_krun-0.1.30-darwin-arm64",
    helperBuildId: "secondbox-microsandbox-helper-0.1.0-darwin-arm64"
  }' >"$materialization"
materialization_digest="$(digest_canonical_json "$materialization")"

export SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1
export SECONDBOX_SCENARIO_COMPUTE_BACKEND=microsandbox
export SECONDBOX_SCENARIO_HOST_PLATFORM=darwin
export SECONDBOX_SCENARIO_NATIVE_RUNNER_BUILD="$build_root"
export SECONDBOX_SCENARIO_NATIVE_ADVERTISED_ADDRESS="$native_advertised_address"
export SECONDBOX_SCENARIO_MICROSANDBOX_BUILD="$build_root"
export SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION="$materialization"
export SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST="$materialization_digest"
export SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST="$runtime_digest"
export SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST="$toolchain_digest"
export SECONDBOX_SCENARIO_ARTIFACT_MANIFEST_DIGEST="$materialization_digest"
export SECONDBOX_SCENARIO_MICROSANDBOX_FLAT_ROOT_DIGEST="$flat_root_digest"
export SECONDBOX_SCENARIO_MICROSANDBOX_SOURCE_OCI_DIGEST="$source_oci_digest"
export SECONDBOX_RUNNER_WORKSPACE_ROOT="$workspace_root"
export TMPDIR=/tmp
export GOTOOLCHAIN=local

"$repo_root/scripts/test-scenario.sh"

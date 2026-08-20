#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ "$(uname -s)" != "Linux" || "$(uname -m)" != "x86_64" ]]; then
  echo "SecondBox Microsandbox Linux scenario requires Linux x86_64" >&2
  exit 1
fi
[[ -c /dev/kvm && -r /dev/kvm && -w /dev/kvm ]] || {
  echo "SecondBox Microsandbox Linux scenario requires readable and writable /dev/kvm" >&2
  exit 1
}
: "${SECONDBOX_MICROSANDBOX_LINUX_BUILD:?set SECONDBOX_MICROSANDBOX_LINUX_BUILD to an exact local build directory}"
build_root="$(realpath -e "$SECONDBOX_MICROSANDBOX_LINUX_BUILD")"
[[ "$build_root" == "$SECONDBOX_MICROSANDBOX_LINUX_BUILD" && ! -L "$build_root" && -d "$build_root" ]] || {
  echo "SECONDBOX_MICROSANDBOX_LINUX_BUILD must be a clean absolute non-symlink directory" >&2
  exit 1
}

helper="$build_root/runtime/bin/secondbox-microsandbox-helper"
agentd="$build_root/runtime/bin/agentd"
firmware="$build_root/runtime/lib/libkrunfw.so.5.6.1"
flat_root="$build_root/rootfs"
evidence="$build_root/build-evidence.txt"
for path in "$helper" "$agentd" "$firmware" "$evidence"; do
  [[ -f "$path" && ! -L "$path" ]] || {
    echo "SecondBox Microsandbox local build is missing a regular input: $path" >&2
    exit 1
  }
done
[[ -x "$helper" && -x "$agentd" && -d "$flat_root" && ! -L "$flat_root" ]] || {
  echo "SecondBox Microsandbox local helper, agentd, or flat root is invalid" >&2
  exit 1
}
grep -Fxq 'upstream_commit=5b335537afad433ad2c0308cb54de13b7015b4e7' "$evidence" || {
  echo "SecondBox Microsandbox local build has the wrong upstream revision" >&2
  exit 1
}
grep -Fxq 'patched_tree=daf8457b13e5f124a63e23a12edbd8482d7da43a' "$evidence" || {
  echo "SecondBox Microsandbox local build has the wrong patched tree" >&2
  exit 1
}
grep -Fxq 'helper_lock_sha256=72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c' "$evidence" || {
  echo "SecondBox Microsandbox local build has the wrong helper lock" >&2
  exit 1
}

temporary="$(mktemp -d /tmp/secondbox-microsandbox-scenario-input.XXXXXX)"
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

runtime_digest="$(digest_text 'secondbox-microsandbox-runtime-v0.6.8-linux-amd64')"
toolchain_digest="$(digest_text 'secondbox-microsandbox-toolchain-v0.6.8-linux-amd64')"
source_oci_digest="$(sed -n 's#^rootfs_image=.*@\(sha256:[a-f0-9]\{64\}\)$#\1#p' "$evidence")"
[[ "$source_oci_digest" =~ ^sha256:[a-f0-9]{64}$ ]] || {
  echo "SecondBox Microsandbox local build lacks a pinned source OCI digest" >&2
  exit 1
}
flat_root_digest="$(cd "$repo_root/runner" && go run ./cmd/secondbox-flat-root-digest "$flat_root")"
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
      guestArchitecture: "amd64",
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
    backendBuildId: "microsandbox-0.6.8-msb_krun-0.1.30",
    helperBuildId: "secondbox-microsandbox-helper-0.1.0"
  }' >"$materialization"
materialization_digest="$(digest_canonical_json "$materialization")"

export SECONDBOX_SCENARIO_COMPUTE_BACKEND=microsandbox
export SECONDBOX_SCENARIO_MICROSANDBOX_BUILD="$build_root"
export SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION="$materialization"
export SECONDBOX_SCENARIO_MICROSANDBOX_MATERIALIZATION_DIGEST="$materialization_digest"
export SECONDBOX_SCENARIO_RUNTIME_BUNDLE_DIGEST="$runtime_digest"
export SECONDBOX_SCENARIO_TOOLCHAIN_BUNDLE_DIGEST="$toolchain_digest"
export SECONDBOX_SCENARIO_ARTIFACT_MANIFEST_DIGEST="$materialization_digest"
export SECONDBOX_SCENARIO_MICROSANDBOX_FLAT_ROOT_DIGEST="$flat_root_digest"
export SECONDBOX_SCENARIO_MICROSANDBOX_SOURCE_OCI_DIGEST="$source_oci_digest"

"$repo_root/scripts/test-scenario.sh"

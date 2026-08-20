#!/usr/bin/env bash
set -euo pipefail

[[ "$(uname -s)" == "Darwin" && "$(uname -m)" == "arm64" ]] || {
  echo "SecondBox macOS Microsandbox qualification requires Apple Silicon macOS" >&2
  exit 1
}
[[ "$(sysctl -n kern.hv_support)" == "1" ]] || {
  echo "SecondBox macOS Microsandbox qualification requires Hypervisor.framework" >&2
  exit 1
}
: "${SECONDBOX_MICROSANDBOX_MACOS_BUILD:?SECONDBOX_MICROSANDBOX_MACOS_BUILD is required}"
build_root="$(cd -- "$SECONDBOX_MICROSANDBOX_MACOS_BUILD" && pwd -P)"
helper="$build_root/runtime/bin/secondbox-microsandbox-helper"
firmware="$build_root/runtime/lib/libkrunfw.5.dylib"
runner="$build_root/runtime/bin/secondbox-runner"
for path in "$helper" "$firmware" "$runner"; do
  [[ -f "$path" ]] || {
    echo "SecondBox macOS Microsandbox qualification artifact is missing: $path" >&2
    exit 1
  }
done
codesign --verify --strict "$helper"
codesign --verify --strict "$firmware"
codesign --verify --strict "$runner"
entitlements="$(codesign -d --entitlements :- "$helper" 2>&1)"
grep -q '<key>com.apple.security.hypervisor</key>' <<<"$entitlements" || {
  echo "SecondBox macOS Microsandbox helper lacks Hypervisor entitlement" >&2
  exit 1
}
linked_libraries="$(otool -L "$helper" | awk 'NR > 1 { print $1 }')"
if grep -Eq '/Users/|/opt/homebrew|/usr/local|libkrunfw' <<<"$linked_libraries"; then
  echo "SecondBox macOS Microsandbox helper has a mutable runtime-library dependency" >&2
  exit 1
fi
export PATH="/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:$PATH"
for tool in e2fsck mke2fs tune2fs; do
  command -v "$tool" >/dev/null || {
    echo "SecondBox macOS Microsandbox qualification requires $tool" >&2
    exit 1
  }
done
qualification_parent="${SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM:-}"
cleanup_parent=""
cleanup_gocache=""
if [[ -z "$qualification_parent" ]]; then
  qualification_parent="$(mktemp -d /tmp/sbx-ms-macos.XXXXXXXX)"
  cleanup_parent="$qualification_parent"
fi
cleanup() {
  if [[ -n "$cleanup_gocache" ]]; then
    rm -rf -- "$cleanup_gocache"
  fi
  if [[ -n "$cleanup_parent" ]]; then
    rm -rf -- "$cleanup_parent"
  fi
}
trap cleanup EXIT
qualification_parent="$(cd -- "$qualification_parent" && pwd -P)"
filesystem_device="$(df -P "$qualification_parent" | awk 'NR == 2 { print $1 }')"
diskutil info "$filesystem_device" | grep -q 'File System Personality:.*APFS' || {
  echo "SecondBox macOS Microsandbox qualification requires APFS" >&2
  exit 1
}
export TMPDIR=/tmp
if [[ -z "${GOCACHE:-}" ]]; then
  GOCACHE="$(mktemp -d /tmp/sbx-go-cache.XXXXXXXX)"
  cleanup_gocache="$GOCACHE"
fi
export GOCACHE
export SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM="$qualification_parent"
export SECONDBOX_MICROSANDBOX_MACOS_BUILD="$build_root"
(
  cd runner
  go test ./cmd/secondbox-runner -run '^$' -count=1
  go test ./internal/microsandbox/... -count=1
)

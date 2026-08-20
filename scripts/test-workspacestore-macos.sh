#!/usr/bin/env bash
set -euo pipefail

[[ "$(uname -s)" == "Darwin" && "$(uname -m)" == "arm64" ]] || {
  echo "SecondBox macOS WorkspaceStore qualification requires Apple Silicon macOS" >&2
  exit 1
}
: "${SECONDBOX_MICROSANDBOX_MACOS_BUILD:?SECONDBOX_MICROSANDBOX_MACOS_BUILD is required}"
build_root="$(cd -- "$SECONDBOX_MICROSANDBOX_MACOS_BUILD" && pwd -P)"
helper="$build_root/runtime/bin/secondbox-microsandbox-helper"
[[ -x "$helper" ]] || {
  echo "SecondBox macOS WorkspaceStore qualification helper is missing: $helper" >&2
  exit 1
}
export PATH="/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:$PATH"
for tool in e2fsck mke2fs tune2fs; do
  command -v "$tool" >/dev/null || {
    echo "SecondBox macOS WorkspaceStore qualification requires $tool" >&2
    exit 1
  }
done
qualification_parent="${SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM:-}"
cleanup_parent=""
cleanup_gocache=""
if [[ -z "$qualification_parent" ]]; then
  qualification_parent="$(mktemp -d /tmp/sbx-ws-macos.XXXXXXXX)"
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
  echo "SecondBox macOS WorkspaceStore qualification requires APFS" >&2
  exit 1
}
if [[ -z "${GOCACHE:-}" ]]; then
  GOCACHE="$(mktemp -d /tmp/sbx-go-cache.XXXXXXXX)"
  cleanup_gocache="$GOCACHE"
fi
export GOCACHE
export SECONDBOX_MICROSANDBOX_HELPER_EXECUTABLE="$helper"
export SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM="$qualification_parent"
export SECONDBOX_REQUIRE_WORKSPACESTORE_QUALIFICATION=1
export SECONDBOX_REQUIRE_PORTABLE_EXT4=1
(
  cd runner
  go test ./internal/workspacestore -count=1
)

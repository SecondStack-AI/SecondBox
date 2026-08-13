#!/usr/bin/env bash
set -euo pipefail

[[ "$(uname -s)" == "Linux" ]] || {
  echo "SecondBox Linux Microsandbox qualification requires Linux" >&2
  exit 1
}
: "${SECONDBOX_MICROSANDBOX_LINUX_BUILD:?set this to the pinned local Microsandbox build}"
: "${SECONDBOX_WORKSPACESTORE_QUALIFICATION_FILESYSTEM:?set this to a qualified reflink filesystem}"
[[ -r /dev/kvm && -w /dev/kvm ]] || {
  echo "SecondBox Linux Microsandbox qualification requires usable /dev/kvm" >&2
  exit 1
}
build_dir="$SECONDBOX_MICROSANDBOX_LINUX_BUILD"
[[ "$build_dir" = /* && -d "$build_dir" ]] || {
  echo "SecondBox Linux Microsandbox build must be an absolute directory" >&2
  exit 1
}
export GOCACHE="${GOCACHE:-$(mktemp -d /tmp/secondbox-microsandbox-gocache.XXXXXXXX)}"
export CARGO_HOME="$build_dir/helper-cargo-home"
export CARGO_TARGET_DIR="$build_dir/cargo-target"
export CARGO_NET_OFFLINE=true

cargo test --locked --manifest-path "$build_dir/source/runner/microsandbox-helper/Cargo.toml"
(
  cd runner
  go test ./cmd/secondbox-runner ./internal/microsandbox/... -count=1
)

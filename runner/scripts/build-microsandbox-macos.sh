#!/usr/bin/env bash
set -euo pipefail

readonly EXPECTED_COMMIT="5b335537afad433ad2c0308cb54de13b7015b4e7"
readonly EXPECTED_TREE="dc506dffd600fcea281bd4ebfc924e1b31afcb2a"
readonly EXPECTED_PATCHED_TREE="daf8457b13e5f124a63e23a12edbd8482d7da43a"
readonly EXPECTED_CARGO_LOCK_SHA256="7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8"
readonly EXPECTED_PATCH_SHA256="4dd2878ec1f760821a6ebd5f23fef6e767382664db2169676e446045f8674756"
readonly EXPECTED_PROBE_LOCK_SHA256="95f0107a1c27f7ad079012a919213207b4256950b73aec33ee624ed33c4638a7"
readonly EXPECTED_HELPER_LOCK_SHA256="72ba8a7f40cc17eb75386425cdb387c46cb394913d30d51b811f08e9f14d681c"
readonly EXPECTED_LIBKRUNFW_COMMIT="21cb6dce19a615f63e41ecb913334d18560c1364"
readonly EXPECTED_KERNEL_TARBALL_SHA256="194eef900ade82df74ed1d695daa45d03ee4bb415cae4f936a3dbaab2dbbb951"
readonly ROOTFS_IMAGE="docker.io/library/alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"
readonly KERNEL_BUILDER_IMAGE="docker.io/library/fedora@sha256:6c75d5bf57cb0fa5aa4b92c6a83c86c791644496d9ac230de7711f5b8ec3b898"

usage() {
  echo "usage: $0 --source <clean-microsandbox-checkout> --output <new-build-directory>" >&2
}

digest() {
  shasum -a 256 "$1" | awk '{print $1}'
}

source_dir=""
output_dir=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      source_dir="$2"
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      output_dir="$2"
      shift 2
      ;;
    *) usage; exit 2 ;;
  esac
done
[[ -n "$source_dir" && -n "$output_dir" ]] || { usage; exit 2; }

[[ "$(uname -s)" == "Darwin" && "$(uname -m)" == "arm64" ]] || {
  echo "SecondBox macOS Microsandbox build requires Apple Silicon" >&2
  exit 1
}
[[ "$(sysctl -n kern.hv_support)" == "1" ]] || {
  echo "SecondBox macOS Microsandbox build requires Hypervisor.framework support" >&2
  exit 1
}
export PATH="/opt/homebrew/opt/e2fsprogs/bin:/opt/homebrew/opt/e2fsprogs/sbin:$PATH"
for tool in cargo codesign docker e2fsck git go make mke2fs otool protoc rustc shasum sysctl tar tune2fs; do
  command -v "$tool" >/dev/null || {
    echo "SecondBox macOS Microsandbox build requires $tool" >&2
    exit 1
  }
done
docker info >/dev/null || {
  echo "SecondBox macOS Microsandbox build requires a running local Docker engine" >&2
  exit 1
}

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
runner_dir="$(cd -- "$script_dir/.." && pwd -P)"
patch_path="$runner_dir/microsandbox-patches/0001-explicit-ext4-uuid-fd-api.patch"

[[ -d "$source_dir/.git" || -f "$source_dir/.git" ]] || {
  echo "SecondBox local Microsandbox source is not a Git checkout: $source_dir" >&2
  exit 1
}
source_dir="$(cd -- "$source_dir" && pwd -P)"
[[ "$(git -C "$source_dir" rev-parse HEAD)" == "$EXPECTED_COMMIT" ]] || {
  echo "SecondBox local Microsandbox source is not at $EXPECTED_COMMIT" >&2
  exit 1
}
[[ -z "$(git -C "$source_dir" status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "SecondBox local Microsandbox source checkout is dirty: $source_dir" >&2
  exit 1
}
[[ "$(git -C "$source_dir" rev-parse 'HEAD^{tree}')" == "$EXPECTED_TREE" ]] || {
  echo "SecondBox local Microsandbox source tree does not match $EXPECTED_TREE" >&2
  exit 1
}
[[ -e "$source_dir/vendor/libkrunfw/.git" ]] || {
  echo "SecondBox local Microsandbox source must have vendor/libkrunfw initialized" >&2
  exit 1
}
[[ "$(git -C "$source_dir/vendor/libkrunfw" rev-parse HEAD)" == "$EXPECTED_LIBKRUNFW_COMMIT" ]] || {
  echo "SecondBox local libkrunfw source is not at $EXPECTED_LIBKRUNFW_COMMIT" >&2
  exit 1
}
[[ -z "$(git -C "$source_dir/vendor/libkrunfw" status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "SecondBox local libkrunfw source checkout is dirty" >&2
  exit 1
}
[[ "$(digest "$source_dir/Cargo.lock")" == "$EXPECTED_CARGO_LOCK_SHA256" ]] || {
  echo "SecondBox local Microsandbox Cargo.lock digest mismatch" >&2
  exit 1
}
[[ "$(digest "$patch_path")" == "$EXPECTED_PATCH_SHA256" ]] || {
  echo "SecondBox local Microsandbox patch digest mismatch" >&2
  exit 1
}
[[ "$(digest "$runner_dir/microsandbox-probe/Cargo.lock")" == "$EXPECTED_PROBE_LOCK_SHA256" ]] || {
  echo "SecondBox Microsandbox probe lock digest mismatch" >&2
  exit 1
}
[[ "$(digest "$runner_dir/microsandbox-helper/Cargo.lock")" == "$EXPECTED_HELPER_LOCK_SHA256" ]] || {
  echo "SecondBox Microsandbox helper lock digest mismatch" >&2
  exit 1
}

output_parent="$(dirname -- "$output_dir")"
mkdir -p -- "$output_parent"
output_parent="$(cd -- "$output_parent" && pwd -P)"
output_dir="$output_parent/$(basename -- "$output_dir")"
[[ ! -e "$output_dir" ]] || {
  echo "SecondBox local Microsandbox output already exists: $output_dir" >&2
  exit 1
}

stage_dir="$(mktemp -d "$output_parent/.secondbox-microsandbox-macos.XXXXXXXX")"
rootfs_container=""
kernel_container=""
cleanup() {
  if [[ -n "${rootfs_container:-}" ]]; then
    docker rm -f "$rootfs_container" >/dev/null 2>&1 || true
  fi
  if [[ -n "${kernel_container:-}" ]]; then
    docker rm -f "$kernel_container" >/dev/null 2>&1 || true
  fi
  if [[ -n "${stage_dir:-}" && -d "$stage_dir" ]]; then
    rm -rf -- "$stage_dir"
  fi
}
trap cleanup EXIT

GIT_CONFIG_GLOBAL=/dev/null git clone --local --no-hardlinks --no-checkout \
  "$source_dir" "$stage_dir/source"
git -C "$stage_dir/source" checkout --detach "$EXPECTED_COMMIT"
git clone --local --no-hardlinks --no-checkout \
  "$source_dir/vendor/libkrunfw" "$stage_dir/source/vendor/libkrunfw"
git -C "$stage_dir/source/vendor/libkrunfw" checkout --detach "$EXPECTED_LIBKRUNFW_COMMIT"
git -C "$stage_dir/source" apply --index --whitespace=error-all "$patch_path"
[[ "$(git -C "$stage_dir/source" write-tree)" == "$EXPECTED_PATCHED_TREE" ]] || {
  echo "SecondBox patched Microsandbox tree does not match $EXPECTED_PATCHED_TREE" >&2
  exit 1
}

cp -R -- "$runner_dir/microsandbox-probe" "$stage_dir/source/secondbox-probe"
mkdir -p -- "$stage_dir/source/runner" "$stage_dir/source/contracts/microsandbox-helper/v1"
cp -R -- "$runner_dir/microsandbox-helper" "$stage_dir/source/runner/microsandbox-helper"
cp -- "$runner_dir/../contracts/microsandbox-helper/v1/helper.proto" \
  "$stage_dir/source/contracts/microsandbox-helper/v1/helper.proto"
mkdir -p -- "$stage_dir/source/build"

docker build --file "$stage_dir/source/secondbox-probe/agentd.Dockerfile" \
  --output "type=local,dest=$stage_dir/agentd-root" "$stage_dir/source"
install -m 0755 "$stage_dir/agentd-root/agentd" "$stage_dir/source/build/agentd"

docker pull "$KERNEL_BUILDER_IMAGE"
kernel_container="secondbox-task8m-kernel-$$"
docker create --name "$kernel_container" "$KERNEL_BUILDER_IMAGE" sleep infinity >/dev/null
docker start "$kernel_container" >/dev/null
docker exec "$kernel_container" mkdir -p /work
docker cp "$stage_dir/source/vendor/libkrunfw/." "$kernel_container:/work"
docker exec --workdir /work "$kernel_container" bash -lc \
  "dnf install -y 'dnf-command(builddep)' python3-pyelftools curl && dnf builddep -y kernel && make -j \"\$(nproc)\" kernel.c"
docker cp "$kernel_container:/work/kernel.c" \
  "$stage_dir/source/vendor/libkrunfw/kernel.c"
mkdir -p -- "$stage_dir/source/vendor/libkrunfw/tarballs"
docker cp "$kernel_container:/work/tarballs/linux-6.12.99.tar.gz" \
  "$stage_dir/source/vendor/libkrunfw/tarballs/linux-6.12.99.tar.gz"
docker rm -f "$kernel_container" >/dev/null
kernel_container=""
make -C "$stage_dir/source/vendor/libkrunfw" -j "$(sysctl -n hw.ncpu)"
[[ "$(digest "$stage_dir/source/vendor/libkrunfw/tarballs/linux-6.12.99.tar.gz")" == \
  "$EXPECTED_KERNEL_TARBALL_SHA256" ]] || {
  echo "SecondBox libkrunfw kernel tarball digest mismatch" >&2
  exit 1
}
install -m 0644 "$stage_dir/source/vendor/libkrunfw/libkrunfw.5.dylib" \
  "$stage_dir/source/build/libkrunfw.5.dylib"

export CARGO_TARGET_DIR="$stage_dir/cargo-target"
CARGO_NET_OFFLINE=false cargo fetch --locked --manifest-path "$stage_dir/source/Cargo.toml"
CARGO_NET_OFFLINE=false cargo fetch --locked \
  --manifest-path "$stage_dir/source/secondbox-probe/Cargo.toml"
CARGO_NET_OFFLINE=false CARGO_HOME="$stage_dir/helper-cargo-home" \
  cargo fetch --locked --manifest-path "$stage_dir/source/runner/microsandbox-helper/Cargo.toml"
CARGO_NET_OFFLINE=true cargo build --locked --manifest-path "$stage_dir/source/Cargo.toml" \
  -p microsandbox-image
CARGO_NET_OFFLINE=true cargo build --locked --manifest-path "$stage_dir/source/Cargo.toml" \
  --no-default-features --features net -p microsandbox-cli
CARGO_NET_OFFLINE=true cargo test --locked \
  --manifest-path "$stage_dir/source/secondbox-probe/Cargo.toml"
CARGO_NET_OFFLINE=true cargo build --locked \
  --manifest-path "$stage_dir/source/secondbox-probe/Cargo.toml" \
  --bin secondbox-microsandbox-probe
CARGO_NET_OFFLINE=true CARGO_HOME="$stage_dir/helper-cargo-home" \
  cargo test --locked --manifest-path "$stage_dir/source/runner/microsandbox-helper/Cargo.toml"
CARGO_NET_OFFLINE=true CARGO_HOME="$stage_dir/helper-cargo-home" \
  cargo build --locked --manifest-path "$stage_dir/source/runner/microsandbox-helper/Cargo.toml" \
  --bin secondbox-microsandbox-helper

mkdir -p -- "$stage_dir/runtime/bin" "$stage_dir/runtime/lib" "$stage_dir/rootfs"
install -m 0755 "$stage_dir/cargo-target/debug/msb" "$stage_dir/runtime/bin/msb"
install -m 0755 "$stage_dir/cargo-target/debug/secondbox-microsandbox-helper" \
  "$stage_dir/runtime/bin/secondbox-microsandbox-helper"
install -m 0755 "$stage_dir/source/build/agentd" "$stage_dir/runtime/bin/agentd"
install -m 0644 "$stage_dir/source/build/libkrunfw.5.dylib" \
  "$stage_dir/runtime/lib/libkrunfw.5.dylib"
ln -s libkrunfw.5.dylib "$stage_dir/runtime/lib/libkrunfw.dylib"
(
  cd -- "$runner_dir"
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 GOCACHE="$stage_dir/go-cache" GOTOOLCHAIN=local \
    go build -trimpath -buildvcs=false \
    -ldflags '-buildid= -X main.releaseVersion=0.0.0-experimental-macos -X main.sourceCommit=local-reviewed' \
    -o "$stage_dir/runtime/bin/secondbox-runner" ./cmd/secondbox-runner
)
"$script_dir/sign-microsandbox-macos.sh" --bundle "$stage_dir/runtime" --identity - \
  >"$stage_dir/signing-evidence.txt"

docker pull "$ROOTFS_IMAGE"
rootfs_container="$(docker create "$ROOTFS_IMAGE")"
docker export "$rootfs_container" | tar -C "$stage_dir/rootfs" -xf -
docker rm "$rootfs_container" >/dev/null
rootfs_container=""
install -m 0755 "$stage_dir/source/build/agentd" "$stage_dir/rootfs/init.secondbox-agentd"

{
  printf 'upstream_commit=%s\n' "$EXPECTED_COMMIT"
  printf 'upstream_tree=%s\n' "$EXPECTED_TREE"
  printf 'patched_tree=%s\n' "$EXPECTED_PATCHED_TREE"
  printf 'cargo_lock_sha256=%s\n' "$EXPECTED_CARGO_LOCK_SHA256"
  printf 'patch_sha256=%s\n' "$EXPECTED_PATCH_SHA256"
  printf 'probe_lock_sha256=%s\n' "$EXPECTED_PROBE_LOCK_SHA256"
  printf 'helper_lock_sha256=%s\n' "$EXPECTED_HELPER_LOCK_SHA256"
  printf 'libkrunfw_commit=%s\n' "$EXPECTED_LIBKRUNFW_COMMIT"
  printf 'kernel_tarball_sha256=%s\n' "$EXPECTED_KERNEL_TARBALL_SHA256"
  printf 'rootfs_image=%s\n' "$ROOTFS_IMAGE"
  printf 'kernel_builder_image=%s\n' "$KERNEL_BUILDER_IMAGE"
  docker image inspect --format 'kernel_builder_repo_digest={{index .RepoDigests 0}}' \
    "$KERNEL_BUILDER_IMAGE"
  printf 'signing_mode=adhoc\n'
  uname -a
  sw_vers
  sysctl kern.hv_support
  diskutil info / | grep -E 'Device Node|File System Personality|Volume Name'
  rustc -Vv
  cargo -V
  go version
  docker version --format 'docker_client={{.Client.Version}} docker_server={{.Server.Version}}'
  codesign -d --entitlements :- "$stage_dir/runtime/bin/secondbox-microsandbox-helper" 2>&1
  shasum -a 256 \
    "$stage_dir/source/build/agentd" \
    "$stage_dir/runtime/bin/msb" \
    "$stage_dir/runtime/bin/secondbox-runner" \
    "$stage_dir/runtime/bin/agentd" \
    "$stage_dir/runtime/lib/libkrunfw.5.dylib" \
    "$stage_dir/cargo-target/debug/secondbox-microsandbox-probe" \
    "$stage_dir/runtime/bin/secondbox-microsandbox-helper"
} >"$stage_dir/build-evidence.txt"

mv -- "$stage_dir" "$output_dir"
stage_dir=""
echo "SecondBox local macOS Microsandbox build ready: $output_dir"

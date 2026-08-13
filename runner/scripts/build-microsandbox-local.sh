#!/usr/bin/env bash
set -euo pipefail

readonly EXPECTED_COMMIT="5b335537afad433ad2c0308cb54de13b7015b4e7"
readonly EXPECTED_TREE="dc506dffd600fcea281bd4ebfc924e1b31afcb2a"
readonly EXPECTED_PATCHED_TREE="2bb82ba33e2175cd7574ffa6d058c6968453fa4b"
readonly EXPECTED_CARGO_LOCK_SHA256="7827c5aad40cfc4ab36be6aba3bc4c0d923e525c50fc4b54741776bcf13b95c8"
readonly EXPECTED_PATCH_SHA256="f38294823f2c8e3b8e7918a8c58b48b0c9c7c521874add5d5985af3d4134eb7c"
readonly EXPECTED_PROBE_LOCK_SHA256="95f0107a1c27f7ad079012a919213207b4256950b73aec33ee624ed33c4638a7"
readonly EXPECTED_HELPER_LOCK_SHA256="3fc5183298bda9579eab9c5f5046c3909bc933d7740a013ce8678135dd504170"
readonly EXPECTED_LIBKRUNFW_COMMIT="21cb6dce19a615f63e41ecb913334d18560c1364"
readonly EXPECTED_KERNEL_TARBALL_SHA256="194eef900ade82df74ed1d695daa45d03ee4bb415cae4f936a3dbaab2dbbb951"
readonly ROOTFS_IMAGE="docker.io/library/alpine@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce"

usage() {
  echo "usage: $0 --source <clean-microsandbox-checkout> --output <new-build-directory>" >&2
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
    *)
      usage
      exit 2
      ;;
  esac
done

[[ -n "$source_dir" && -n "$output_dir" ]] || { usage; exit 2; }

for tool in cargo docker git make nproc rustc sha256sum tar uv; do
  command -v "$tool" >/dev/null || {
    echo "SecondBox local Microsandbox build requires $tool" >&2
    exit 1
  }
done

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
runner_dir="$(cd -- "$script_dir/.." && pwd -P)"
patch_path="$runner_dir/microsandbox-patches/0001-explicit-ext4-uuid-fd-api.patch"

[[ -d "$source_dir/.git" || -f "$source_dir/.git" ]] || {
  echo "SecondBox local Microsandbox source is not a Git checkout: $source_dir" >&2
  exit 1
}
source_dir="$(cd -- "$source_dir" && pwd -P)"

actual_commit="$(git -C "$source_dir" rev-parse HEAD)"
[[ "$actual_commit" == "$EXPECTED_COMMIT" ]] || {
  echo "SecondBox local Microsandbox source commit is $actual_commit, expected $EXPECTED_COMMIT" >&2
  exit 1
}
[[ -z "$(git -C "$source_dir" status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "SecondBox local Microsandbox source checkout is dirty: $source_dir" >&2
  exit 1
}
actual_tree="$(git -C "$source_dir" rev-parse 'HEAD^{tree}')"
[[ "$actual_tree" == "$EXPECTED_TREE" ]] || {
  echo "SecondBox local Microsandbox source tree is $actual_tree, expected $EXPECTED_TREE" >&2
  exit 1
}

[[ -e "$source_dir/vendor/libkrunfw/.git" ]] || {
  echo "SecondBox local Microsandbox source must have vendor/libkrunfw initialized" >&2
  exit 1
}
actual_libkrunfw_commit="$(git -C "$source_dir/vendor/libkrunfw" rev-parse HEAD)"
[[ "$actual_libkrunfw_commit" == "$EXPECTED_LIBKRUNFW_COMMIT" ]] || {
  echo "SecondBox libkrunfw source commit is $actual_libkrunfw_commit, expected $EXPECTED_LIBKRUNFW_COMMIT" >&2
  exit 1
}
[[ -z "$(git -C "$source_dir/vendor/libkrunfw" status --porcelain=v1 --untracked-files=all)" ]] || {
  echo "SecondBox local libkrunfw source checkout is dirty: $source_dir/vendor/libkrunfw" >&2
  exit 1
}

actual_lock_sha256="$(sha256sum "$source_dir/Cargo.lock" | awk '{print $1}')"
[[ "$actual_lock_sha256" == "$EXPECTED_CARGO_LOCK_SHA256" ]] || {
  echo "SecondBox local Microsandbox Cargo.lock digest is $actual_lock_sha256, expected $EXPECTED_CARGO_LOCK_SHA256" >&2
  exit 1
}
actual_patch_sha256="$(sha256sum "$patch_path" | awk '{print $1}')"
[[ "$actual_patch_sha256" == "$EXPECTED_PATCH_SHA256" ]] || {
  echo "SecondBox local Microsandbox patch digest is $actual_patch_sha256, expected $EXPECTED_PATCH_SHA256" >&2
  exit 1
}
actual_probe_lock_sha256="$(sha256sum "$runner_dir/microsandbox-probe/Cargo.lock" | awk '{print $1}')"
[[ "$actual_probe_lock_sha256" == "$EXPECTED_PROBE_LOCK_SHA256" ]] || {
  echo "SecondBox Microsandbox probe lock digest is $actual_probe_lock_sha256, expected $EXPECTED_PROBE_LOCK_SHA256" >&2
  exit 1
}
actual_helper_lock_sha256="$(sha256sum "$runner_dir/microsandbox-helper/Cargo.lock" | awk '{print $1}')"
[[ "$actual_helper_lock_sha256" == "$EXPECTED_HELPER_LOCK_SHA256" ]] || {
  echo "SecondBox Microsandbox helper lock digest is $actual_helper_lock_sha256, expected $EXPECTED_HELPER_LOCK_SHA256" >&2
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

stage_dir="$(mktemp -d "$output_parent/.secondbox-microsandbox-build.XXXXXXXX")"
rootfs_container=""
cleanup() {
  if [[ -n "${rootfs_container:-}" ]]; then
    docker rm -f "$rootfs_container" >/dev/null 2>&1 || true
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

actual_patched_tree="$(git -C "$stage_dir/source" write-tree)"
[[ "$actual_patched_tree" == "$EXPECTED_PATCHED_TREE" ]] || {
  echo "SecondBox patched Microsandbox tree is $actual_patched_tree, expected $EXPECTED_PATCHED_TREE" >&2
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

(
  cd -- "$stage_dir/source/vendor/libkrunfw"
  uv run --with pyelftools==0.32 make -j "$(nproc)"
)
actual_kernel_tarball_sha256="$(
  sha256sum "$stage_dir/source/vendor/libkrunfw/tarballs/linux-6.12.99.tar.gz" | awk '{print $1}'
)"
[[ "$actual_kernel_tarball_sha256" == "$EXPECTED_KERNEL_TARBALL_SHA256" ]] || {
  echo "SecondBox libkrunfw kernel tarball digest is $actual_kernel_tarball_sha256, expected $EXPECTED_KERNEL_TARBALL_SHA256" >&2
  exit 1
}
install -m 0644 "$stage_dir/source/vendor/libkrunfw/libkrunfw.so.5.6.1" \
  "$stage_dir/source/build/libkrunfw.so.5.6.1"

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
  rustc -Vv
  cargo -V
  docker version --format 'docker_client={{.Client.Version}} docker_server={{.Server.Version}}'
  uv --version
} >"$stage_dir/build-evidence.txt"

CARGO_NET_OFFLINE=true CARGO_TARGET_DIR="$stage_dir/cargo-target" \
  cargo build --locked --manifest-path "$stage_dir/source/Cargo.toml" -p microsandbox-image
CARGO_NET_OFFLINE=true CARGO_TARGET_DIR="$stage_dir/cargo-target" \
  cargo build --locked --offline --manifest-path "$stage_dir/source/Cargo.toml" \
  --no-default-features --features net -p microsandbox-cli
CARGO_NET_OFFLINE=true CARGO_TARGET_DIR="$stage_dir/cargo-target" \
  cargo test --locked --manifest-path "$stage_dir/source/secondbox-probe/Cargo.toml"
CARGO_NET_OFFLINE=true CARGO_TARGET_DIR="$stage_dir/cargo-target" \
  cargo build --locked --manifest-path "$stage_dir/source/secondbox-probe/Cargo.toml" \
  --bin secondbox-microsandbox-probe
CARGO_HOME="$stage_dir/helper-cargo-home" \
  cargo fetch --locked --manifest-path "$stage_dir/source/runner/microsandbox-helper/Cargo.toml"
CARGO_NET_OFFLINE=true CARGO_HOME="$stage_dir/helper-cargo-home" \
  CARGO_TARGET_DIR="$stage_dir/cargo-target" \
  cargo test --locked --manifest-path "$stage_dir/source/runner/microsandbox-helper/Cargo.toml"
CARGO_NET_OFFLINE=true CARGO_HOME="$stage_dir/helper-cargo-home" \
  CARGO_TARGET_DIR="$stage_dir/cargo-target" \
  cargo build --locked --manifest-path "$stage_dir/source/runner/microsandbox-helper/Cargo.toml" \
  --bin secondbox-microsandbox-helper

mkdir -p -- "$stage_dir/runtime/bin" "$stage_dir/runtime/lib" "$stage_dir/rootfs"
install -m 0755 "$stage_dir/cargo-target/debug/msb" "$stage_dir/runtime/bin/msb"
install -m 0755 "$stage_dir/cargo-target/debug/secondbox-microsandbox-helper" \
  "$stage_dir/runtime/bin/secondbox-microsandbox-helper"
install -m 0755 "$stage_dir/source/build/agentd" "$stage_dir/runtime/bin/agentd"
install -m 0644 "$stage_dir/source/build/libkrunfw.so.5.6.1" \
  "$stage_dir/runtime/lib/libkrunfw.so.5.6.1"
ln -s libkrunfw.so.5.6.1 "$stage_dir/runtime/lib/libkrunfw.so.5"
ln -s libkrunfw.so.5 "$stage_dir/runtime/lib/libkrunfw.so"

docker pull "$ROOTFS_IMAGE"
rootfs_container="$(docker create "$ROOTFS_IMAGE")"
docker export "$rootfs_container" | tar -C "$stage_dir/rootfs" -xf -
docker rm "$rootfs_container" >/dev/null
rootfs_container=""
install -m 0755 "$stage_dir/source/build/agentd" "$stage_dir/rootfs/init.secondbox-agentd"

{
  sha256sum "$stage_dir/source/build/agentd"
  sha256sum "$stage_dir/runtime/bin/msb"
  sha256sum "$stage_dir/runtime/bin/agentd"
  sha256sum "$stage_dir/runtime/lib/libkrunfw.so.5.6.1"
  sha256sum "$stage_dir/cargo-target/debug/secondbox-microsandbox-probe"
  sha256sum "$stage_dir/runtime/bin/secondbox-microsandbox-helper"
} >>"$stage_dir/build-evidence.txt"

mv -- "$stage_dir" "$output_dir"
stage_dir=""
echo "SecondBox local Microsandbox build ready: $output_dir"

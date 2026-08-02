#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
local_root="${SECONDBOX_STRESS_LOCAL_ROOT:-$repo_root/.secondbox/stress}"
artifacts_dir="$local_root/artifacts"
trust_dir="$local_root/trust"
private_key="$trust_dir/manifest-private.pem"
public_key="$trust_dir/manifest-public.pem"
fingerprint_file="$trust_dir/manifest-public.sha256"
config_file="$local_root/stress.json"
workspace_root="$local_root/workspaces"
results_dir="$local_root/results"
verify_script="$repo_root/runner/scripts/microvm-image/verify.sh"

fail() {
  echo "SecondBox local stress preparation failed: $*" >&2
  exit 1
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  cat <<'USAGE'
Usage: just prepare-stress

Builds and signs a reusable local Firecracker artifact bundle under
.secondbox/stress. A valid existing bundle is verified and reused.

Optional environment:
  SECONDBOX_STRESS_LOCAL_ROOT  Absolute persistent preparation directory.
USAGE
  exit 0
fi
[[ "$#" -eq 0 ]] || fail "unexpected argument: $1"

for command in blkid debugfs findmnt install jq openssl realpath sha256sum; do
  command -v "$command" >/dev/null 2>&1 ||
    fail "missing required command: $command"
done
[[ "$(uname -m)" == "x86_64" ]] ||
  fail "the Firecracker artifact builder currently requires an x86-64 host"
[[ "$local_root" == /* && "$local_root" != "/" ]] ||
  fail "SECONDBOX_STRESS_LOCAL_ROOT must be a narrow absolute path"

mkdir -p "$local_root" "$trust_dir" "$workspace_root" "$results_dir"
[[ ! -L "$local_root" && "$(realpath -e "$local_root")" == "$local_root" ]] ||
  fail "SECONDBOX_STRESS_LOCAL_ROOT must be a clean non-symlink path"
for directory in "$trust_dir" "$workspace_root" "$results_dir"; do
  [[ ! -L "$directory" && -d "$directory" ]] ||
    fail "local stress directory must not be a symbolic link: $directory"
done

workspace_fstype="$(findmnt -T "$workspace_root" -n -o FSTYPE)" ||
  fail "findmnt could not resolve the local Workspace parent"
[[ "$workspace_fstype" == "xfs" || "$workspace_fstype" == "btrfs" ]] ||
  fail "local Workspace parent must be on XFS or Btrfs, got $workspace_fstype"

if [[ ! -e "$config_file" ]]; then
  install -m 0600 "$repo_root/scripts/stress-config.example.json" "$config_file"
elif [[ -L "$config_file" || ! -f "$config_file" ]]; then
  fail "local stress config must be a regular non-symlink file: $config_file"
fi

public_key_fingerprint() {
  openssl pkey -pubin -in "$public_key" -outform DER 2>/dev/null |
    sha256sum |
    awk '{print $1}'
}

if [[ -d "$artifacts_dir" ]]; then
  [[ ! -L "$artifacts_dir" ]] ||
    fail "local artifact directory must not be a symbolic link"
  [[ -f "$public_key" && ! -L "$public_key" ]] ||
    fail "prepared artifacts require the saved trust anchor: $public_key"
  public_key_sha256="$(public_key_fingerprint)" ||
    fail "saved artifact public key is invalid"
  "$verify_script" "$artifacts_dir" "$public_key" "$public_key_sha256"
  printf '%s\n' "$public_key_sha256" >"$fingerprint_file"
  chmod 0600 "$fingerprint_file"
  echo "SecondBox local stress bundle is already prepared and verified:"
  echo "  artifacts: $artifacts_dir"
  echo "  trust key: $public_key"
  echo "  workspace: $workspace_root"
  echo "Run: just test-stress"
  exit 0
elif [[ -e "$artifacts_dir" || -L "$artifacts_dir" ]]; then
  fail "local artifact target must be absent or an existing directory: $artifacts_dir"
fi

for command in \
  bc bison cmp curl debootstrap docker fakeroot flex gcc getconf git go make \
  mkfs.ext4 mktemp python3 sudo tar xz; do
  command -v "$command" >/dev/null 2>&1 ||
    fail "missing build command: $command"
done
docker info >/dev/null 2>&1 ||
  fail "Docker Engine is not available to the current user"

if [[ ! -e "$private_key" && ! -e "$public_key" ]]; then
  echo "Generating persistent local artifact signing key"
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 \
    -out "$private_key" >/dev/null 2>&1
  chmod 0600 "$private_key"
  openssl pkey -in "$private_key" -pubout -out "$public_key" >/dev/null 2>&1
  chmod 0644 "$public_key"
elif [[ -f "$private_key" && ! -L "$private_key" && ! -e "$public_key" ]]; then
  openssl pkey -in "$private_key" -pubout -out "$public_key" >/dev/null 2>&1
  chmod 0644 "$public_key"
fi
[[ -f "$private_key" && ! -L "$private_key" ]] ||
  fail "building a bundle requires the saved private key: $private_key"
[[ -f "$public_key" && ! -L "$public_key" ]] ||
  fail "building a bundle requires the saved public key: $public_key"

derived_public_key="$(mktemp "$trust_dir/derived-public.XXXXXX")"
openssl pkey -in "$private_key" -pubout -out "$derived_public_key" >/dev/null 2>&1 ||
  fail "saved artifact private key is invalid"
if ! cmp -s "$derived_public_key" "$public_key"; then
  rm -f "$derived_public_key"
  fail "saved artifact public key does not match the private key"
fi
rm -f "$derived_public_key"

public_key_sha256="$(public_key_fingerprint)" ||
  fail "saved artifact public key is invalid"
printf '%s\n' "$public_key_sha256" >"$fingerprint_file"
chmod 0600 "$fingerprint_file"

mkdir -p "$repo_root/.tmp"
build_root="$(mktemp -d "$repo_root/.tmp/prepare-stress.XXXXXX")"
cleanup() {
  local latest_link="$repo_root/runner/releases/microvm/latest"
  if [[ -L "$latest_link" && "$(readlink "$latest_link")" == "$build_root/artifacts" ]]; then
    rm "$latest_link"
    rmdir "$repo_root/runner/releases/microvm" "$repo_root/runner/releases" 2>/dev/null || true
  fi
  rm -rf "$build_root"
}
trap cleanup EXIT

rootfs_source="$build_root/rootfs-source"
staged_artifacts="$build_root/artifacts"
kernel_jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)"
artifact_version="local-$(git -C "$repo_root" rev-parse --short=12 HEAD)"

echo "Preparing the standard SecondBox guest rootfs"
env \
  -u SECONDBOX_RUNNER_MICROVM_OCI_BASE_REFERENCE \
  -u SECONDBOX_RUNNER_MICROVM_OCI_MODE \
  SECONDBOX_RUNNER_MICROVM_IMAGE_DEFINITION="$repo_root/runner/scripts/microvm-image/rootfs/secondbox-debian-image-definition.json" \
  SECONDBOX_RUNNER_MICROVM_BROWSER_POLICY=forbid \
  SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR="$rootfs_source" \
  "$repo_root/runner/scripts/microvm-image/rootfs/build-secondbox-rootfs-source.sh"

echo "Building and signing the reusable SecondBox microVM bundle"
SECONDBOX_RUNNER_MICROVM_BUILD_KERNEL=true \
SECONDBOX_RUNNER_MICROVM_KERNEL_PATH= \
SECONDBOX_RUNNER_MICROVM_KERNEL_CONFIG= \
SECONDBOX_RUNNER_MICROVM_ARTIFACT_VERSION="$artifact_version" \
SECONDBOX_RUNNER_MICROVM_OUT_DIR="$staged_artifacts" \
SECONDBOX_RUNNER_MICROVM_ROOTFS_SOURCE_DIR="$rootfs_source" \
SECONDBOX_RUNNER_MICROVM_SIGNING_KEY="$private_key" \
SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY="$public_key" \
SECONDBOX_RUNNER_MICROVM_PUBLIC_KEY_SHA256="$public_key_sha256" \
SECONDBOX_RUNNER_MICROVM_ROOTFS_SIZE_MIB=10240 \
SECONDBOX_RUNNER_MICROVM_SHARED_SIZE_MIB=1024 \
SECONDBOX_RUNNER_MICROVM_SHARED_FORMAT=ext4 \
SECONDBOX_RUNNER_MICROVM_ROOTFS_UUID=5345434f-4e44-424f-5800-000000000001 \
SECONDBOX_RUNNER_MICROVM_KERNEL_LOCK="$repo_root/runner/scripts/microvm-image/kernel.lock" \
SECONDBOX_RUNNER_MICROVM_KERNEL_CACHE="$local_root/cache/kernel" \
SECONDBOX_RUNNER_MICROVM_KERNEL_JOBS="$kernel_jobs" \
SECONDBOX_RUNNER_MICROVM_KERNEL_VERIFY_ONLY=false \
  "$repo_root/runner/scripts/microvm-image/build.sh"

rm -rf "$staged_artifacts/kernel-build"
"$verify_script" "$staged_artifacts" "$public_key" "$public_key_sha256"
mv "$staged_artifacts" "$artifacts_dir"

echo "SecondBox local stress bundle prepared and verified:"
echo "  artifacts: $artifacts_dir"
echo "  trust key: $public_key"
echo "  workspace: $workspace_root"
echo "Run: just test-stress"

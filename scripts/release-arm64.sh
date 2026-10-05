#!/usr/bin/env bash
set -Eeuo pipefail
# Stages the arm64 artifact set of a release on a Linux arm64 KVM host: scenario
# qualification on this host, then arm64 images, bundle and standard Profiles
# beside the architecture-neutral files of the final amd64 release of HEAD.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
fail() { echo "SecondBox arm64 release: $*" >&2; exit 1; }
[[ "$#" -eq 2 ]] || fail 'usage: just release-arm64 VERSION AMD64_RELEASE_DIR'
version="$1"
shared="$2"
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || fail 'VERSION must be SemVer without v or build metadata'
[[ "$(uname -s)/$(uname -m)" == Linux/aarch64 ]] || fail 'requires a Linux arm64 host'
config="${RELEASE_ENV_FILE:-${HOME:?}/.config/secondbox/release.env}"
[[ -f "$config" ]] || fail "copy deploy/release-arm64.env.example to $config and configure it"
bash -n "$config"
set -a
source "$config"
set +a
: "${RELEASE_GOROOT:?set RELEASE_GOROOT}"
export GOROOT="$RELEASE_GOROOT"
export GOTOOLCHAIN="go$(awk '$1 == "go" {print $2; exit}' go.mod)"
export PATH="$GOROOT/bin:/usr/bin:/bin:$PATH"
[[ -x /usr/bin/just && "$(go env GOVERSION)" == "$GOTOOLCHAIN" ]] || fail 'release toolchain is absent or differs from go.mod'
for tool in git jq docker sha256sum openssl npm node flock strings; do
  command -v "$tool" >/dev/null || fail "missing tool: $tool"
done
[[ -z "$(git status --porcelain --untracked-files=all)" ]] || fail 'requires a clean tree including untracked files'
[[ "$(git symbolic-ref --short HEAD)" == main ]] || fail 'HEAD must be on main'
source_commit="$(git rev-parse HEAD)"
tag="v$version"
if git show-ref --verify --quiet "refs/tags/$tag"; then
  [[ "$(git rev-parse "refs/tags/$tag^{commit}")" == "$source_commit" ]] || fail "$tag already identifies another commit"
fi
for key in SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY RELEASE_OUTPUT_ROOT; do
  [[ "${!key:-}" == /* && -e "${!key}" && ! -L "${!key}" ]] || fail "$key must be an existing absolute non-symlink path"
done
actual="$(openssl pkey -pubin -in "$SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY" -outform DER | sha256sum | cut -d' ' -f1)"
[[ "$actual" == "${SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256:-}" ]] || fail 'release public key fingerprint mismatch'
[[ -d "$shared" && ! -L "$shared" ]] || fail 'AMD64_RELEASE_DIR must be a non-symlink directory'
shared="$(cd "$shared" && pwd)"
output="$RELEASE_OUTPUT_ROOT/$version-arm64"
[[ ! -e "$output" && ! -L "$output" ]] || fail "output already exists: $output"
for device in /dev/kvm /dev/net/tun; do
  [[ -c "$device" && -r "$device" && -w "$device" ]] || fail "requires accessible $device"
done
: "${RELEASE_BUILDX_BUILDER:?set RELEASE_BUILDX_BUILDER to an operator-owned release builder}"
docker buildx inspect "$RELEASE_BUILDX_BUILDER" >/dev/null
mkdir -p .tmp/release
exec 8>.tmp/release/checkout.lock
flock -n 8 || fail 'another release owns this checkout'
git show-ref --verify --quiet "refs/tags/$tag" || git tag "$tag" "$source_commit"
directory="$repo_root/.tmp/release/$(date -u +%Y%m%dT%H%M%S)-$$-arm64"
mkdir -m 700 "$directory"
echo "arm64 release run ($source_commit); logs: $directory"
/usr/bin/just test-scenario >"$directory/qualification.log" 2>&1 || fail "scenario qualification failed; inspect $directory/qualification.log"
env BUILDX_BUILDER="$RELEASE_BUILDX_BUILDER" RELEASE_GUEST_ARCHITECTURE=arm64 RELEASE_IMAGE_PLATFORMS=linux/arm64 \
  scripts/release-stage.sh --shared-from "$shared" "$version" "$output" >"$directory/stage.log" 2>&1 || fail "staging failed; inspect $directory/stage.log"
[[ "$(git rev-parse HEAD)" == "$source_commit" && -z "$(git status --porcelain --untracked-files=all)" ]] || fail 'source changed during release'
printf 'Staged arm64 artifact set: %s\nAfter the tag push, upload it from this host:\n' "$output"
printf '/usr/bin/just release-upload %q %q\n' "$version" "$output"

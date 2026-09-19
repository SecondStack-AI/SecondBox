#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail() { echo "SecondBox gVisor preparation: $*" >&2; exit 1; }
[[ $# == 0 || ( $# == 1 && "$1" == --preflight ) ]] || fail 'usage: prepare-gvisor-qualification.sh [--preflight]'

: "${QUALIFY_GVISOR_HOST_BUILD_ROOT:?set QUALIFY_GVISOR_HOST_BUILD_ROOT}"
root="$QUALIFY_GVISOR_HOST_BUILD_ROOT"
[[ "$(uname -s)/$(uname -m)" == Linux/x86_64 ]] || fail 'requires Linux x86_64'
for tool in docker jq realpath flock sha512sum; do
  command -v "$tool" >/dev/null || fail "missing tool: $tool"
done
[[ "$root" == /* && "$(dirname "$root")" != / && "$(realpath -m -- "$root")" == "$root" && "$root" != *','* ]] ||
  fail 'build root must be a clean absolute non-symlink path below an operator directory (without commas)'
owner='secondbox.gvisor-qualification/v1'
check_root() {
  [[ -d "$root" && ! -L "$root" && "$(realpath -e -- "$root")" == "$root" && -O "$root" && "$(stat -c %a "$root")" == 700 &&
     -f "$root/.owner" && ! -L "$root/.owner" && "$(cat "$root/.owner")" == "$owner" &&
     -f "$root/prepare.lock" && ! -L "$root/prepare.lock" ]] ||
    fail 'build root must be owned by this user and managed by this script; select a new dedicated path'
}
if [[ -e "$root" || -L "$root" ]]; then
  check_root
else
  ancestor="$(dirname "$root")"
  while [[ ! -e "$ancestor" ]]; do ancestor="$(dirname "$ancestor")"; done
  [[ -d "$ancestor" && -w "$ancestor" ]] || fail 'build root ancestor must be writable'
fi
docker info >/dev/null || fail 'Docker unavailable'
docker buildx version >/dev/null || fail 'Docker Buildx unavailable'
[[ "${1:-}" != --preflight ]] || exit 0

# Publish ownership and the lock inode together, including on a concurrent first run.
if [[ ! -e "$root" ]]; then
  mkdir -p -- "$(dirname "$root")"
  initial="$(mktemp -d "$(dirname "$root")/.secondbox-gvisor-owner.XXXXXXXX")"
  printf '%s\n' "$owner" >"$initial/.owner"
  touch "$initial/prepare.lock"
  # Some coreutils versions return failure when a concurrent initializer wins.
  mv -T -n -- "$initial" "$root" || check_root
  if [[ -d "$initial" ]]; then rm -rf -- "$initial"; fi
fi
check_root
exec 7>"$root/prepare.lock"
while :; do
  if flock -w 30 7; then break; else status=$?; fi
  [[ "$status" == 1 ]] || fail "build-root lock failed ($status)"
  echo 'SecondBox gVisor preparation: waiting for build-root owner' >&2
done

temporary="$(mktemp -d "$root/.prepare.XXXXXXXX")"
trap 'rm -rf -- "$temporary"' EXIT
# Reuse the release asset builder, including its reviewed source image, runsc
# fetcher, guest-agent build, mount targets, and materialization verifiers.
# BuildKit invalidates the guest build on source changes, including dirty trees.
docker buildx build --platform linux/amd64 --provenance=false --sbom=false \
  --file "$repo_root/runner/deploy/gvisor-artifact-transport.Dockerfile" --target assemble \
  --build-arg RELEASE_VERSION=qualification --build-arg "SOURCE_COMMIT=$(git -C "$repo_root" rev-parse HEAD)" \
  --load --iidfile "$temporary/image-id" "$repo_root" >&2
image="$(cat "$temporary/image-id")"
[[ "$image" =~ ^sha256:[a-f0-9]{64}$ ]] || fail 'builder returned an invalid image identity'
build="$root/${image#sha256:}"
pinned="$(sed -n 's/^readonly RUNSC_SHA512="\([a-f0-9]\{128\}\)"$/\1/p' "$repo_root/runner/scripts/fetch-runsc.sh")"
[[ -n "$pinned" ]] || fail 'cannot read reviewed runsc pin'

# Only this explicitly bound directory is writable. No privileged container,
# host devices, or host process namespace is needed to preserve numeric owners.
# The original image supplies the expected digests; cached host metadata cannot
# bless corrupted bytes. Retain every published image directory for its readers.
docker run --rm -i --network none --user 0:0 --entrypoint /bin/sh \
  --mount "type=bind,src=$root,dst=/output" "$image" -eu -s -- "${image#sha256:}" "$pinned" <<'PREPARE' >&2 || fail 'artifact extraction or verification failed'
key="$1" pinned="$2"
expected=/secondbox-runner-gvisor
verify() {
  actual="$1"
  test -d "$actual" && test ! -L "$actual"
  test -d "$actual/bin" && test ! -L "$actual/bin"
  test -d "$actual/rootfs" && test ! -L "$actual/rootfs"
  for file in bin/runsc bin/secondbox-guest-agent bin/secondbox-flat-root-digest bin/secondbox-materialization-digest materialization.json identity.json SHA256SUMS; do
    test -f "$actual/$file" && test ! -L "$actual/$file"
  done
  (cd "$actual" && sha256sum -c "$expected/SHA256SUMS")
  cmp "$expected/SHA256SUMS" "$actual/SHA256SUMS"
  test "$(sha512sum "$actual/bin/runsc" | cut -d' ' -f1)" = "$pinned"
  test -x "$actual/bin/runsc" && test -x "$actual/bin/secondbox-guest-agent"
  test "$(secondbox-flat-root-digest "$actual/rootfs")" = "$(jq -er .flatRootDigest "$expected/identity.json")"
  test "$(secondbox-materialization-digest "$actual/materialization.json")" = "$(jq -er .materializationDigest "$expected/identity.json")"
}
if test -e "/output/$key" || test -L "/output/$key"; then
  verify "/output/$key"
else
  stage="$(mktemp -d /output/.extract.XXXXXXXX)"
  trap 'rm -rf -- "$stage"' EXIT
  cp -a "$expected" "$stage/build"
  verify "$stage/build"
  mv -T "$stage/build" "/output/$key"
fi
PREPARE
printf '%s\n' "$build"

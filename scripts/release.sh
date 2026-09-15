#!/usr/bin/env bash
set -Eeuo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
fail() { echo "SecondBox release: $*" >&2; exit 1; }
full=false
resume=false
version=''
for argument; do
  case "$argument" in
    --full) full=true ;;
    --resume) resume=true ;;
    *) [[ -z "$version" ]] || fail 'usage: just release VERSION [--full] [--resume]'; version="$argument" ;;
  esac
done
[[ -n "$version" ]] || fail 'usage: just release VERSION [--full] [--resume]'
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || fail 'VERSION must be SemVer without v or build metadata'
config="${RELEASE_ENV_FILE:-${HOME:?}/.config/secondbox/release.env}"
[[ -f "$config" ]] || fail "copy deploy/release.env.example to $config and configure it"
bash -n "$config"
set -a
source "$config"
set +a
tier=release
installer_flags=()
export RELEASE_IMAGE_PLATFORMS=linux/amd64
if $full; then
  tier=nightly
  installer_flags=(--full)
  export RELEASE_IMAGE_PLATFORMS=linux/amd64,linux/arm64
fi
# Pin the reviewed release toolchain even when invoked through a mise shim.
: "${RELEASE_GOROOT:?set RELEASE_GOROOT}"
: "${RELEASE_PROTOC_BIN:?set RELEASE_PROTOC_BIN}"
export GOROOT="$RELEASE_GOROOT"
export GOTOOLCHAIN="go$(awk '$1 == "go" {print $2; exit}' go.mod)"
export PATH="$GOROOT/bin:$RELEASE_PROTOC_BIN:/usr/bin:/bin:$PATH"
[[ -x /usr/bin/just && -x "$GOROOT/bin/go" && -x "$RELEASE_PROTOC_BIN/protoc" ]] || fail 'release toolchain is absent'
[[ "$(go env GOVERSION)" == "$GOTOOLCHAIN" ]] || fail 'release Go version differs from go.mod'
protoc_version="$(sed -n 's/^version="\([^"]*\)"$/\1/p' scripts/install-protoc.sh)"
[[ "$(protoc --version)" == "libprotoc $protoc_version" ]] || fail 'release protoc differs from repository pin'
for tool in git jq docker virsh virt-install qemu-img cloud-localds ssh scp ssh-keygen sha256sum findmnt openssl python3 getent setfacl npm flock; do
  command -v "$tool" >/dev/null || fail "missing tool: $tool"
done
[[ -z "$(git status --porcelain --untracked-files=all)" ]] || fail 'requires a clean tree including untracked files'
[[ "$(git symbolic-ref --short HEAD)" == main ]] || fail 'HEAD must be on main'
source_commit="$(git rev-parse HEAD)"
tag="v$version"
if git show-ref --verify --quiet "refs/tags/$tag"; then
  [[ "$(git rev-parse "refs/tags/$tag^{commit}")" == "$source_commit" ]] || fail "$tag already identifies another commit"
fi
for key in SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY SECONDBOX_INSTALLER_QUALIFICATION_IMAGE SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT RELEASE_OUTPUT_ROOT; do
  [[ "${!key:-}" == /* && -e "${!key}" && ! -L "${!key}" ]] || fail "$key must be an existing absolute non-symlink path"
done
[[ -d "$RELEASE_OUTPUT_ROOT" && -d "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT" && -d "$SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR" ]] || fail 'invalid directory inputs'
[[ "${SECONDBOX_RELEASE_POSTGRES_IMAGE:-}" =~ ^docker.io/library/postgres@sha256:[a-f0-9]{64}$ ]] || fail 'Postgres image must be digest pinned'
[[ "${SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256:-}" =~ ^[a-f0-9]{64}$ ]] || fail 'invalid release public key fingerprint'
actual="$(openssl pkey -pubin -in "$SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY" -outform DER | sha256sum | cut -d' ' -f1)"
[[ "$actual" == "$SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256" ]] || fail 'release public key fingerprint mismatch'
[[ "${SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256:-}" == "$(scripts/prepare-installer-qualification-image.sh --print-sha256)" ]] || fail 'installer image pin differs from repository'
printf '%s  %s\n' "$SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256" "$SECONDBOX_INSTALLER_QUALIFICATION_IMAGE" | sha256sum --check --status || fail 'installer image digest mismatch'
workspace_type="$(findmnt -n -o FSTYPE --target "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT")"
[[ "$workspace_type" == btrfs || "$workspace_type" == xfs ]] || fail 'installer workspace must be on Btrfs or XFS'
# Bind only the evidence produced in this checkout by this qualification.
export SECONDBOX_GVISOR_QUALIFICATION_EVIDENCE="$repo_root/.tmp/gvisor-linux-scenario-qualification-evidence.json"
export SECONDBOX_GVISOR_POD_QUALIFICATION_EVIDENCE="$repo_root/.tmp/gvisor-pod-linux-scenario-qualification-evidence.json"
build="$RELEASE_OUTPUT_ROOT/$version-build"
candidate="$RELEASE_OUTPUT_ROOT/$version-candidate"
output="$RELEASE_OUTPUT_ROOT/$version"
# --resume reuses a retained checksummed build and commit-exact scenario evidence
# after a run failed in a gate; only the gates are requalified.
if $resume; then
  [[ -d "$build" && ! -L "$build" && -f "$build/.release-build.json" ]] || fail "resume requires a retained build: $build"
  for evidence in .tmp/scenario-qualification-evidence.json "$SECONDBOX_GVISOR_QUALIFICATION_EVIDENCE"; do
    [[ "$(jq -r '.sourceCommit' "$evidence" 2>/dev/null)" == "$source_commit" ]] || fail "resume requires commit-exact evidence: $evidence"
  done
  ! $full || [[ "$(jq -r '.sourceCommit' "$SECONDBOX_GVISOR_POD_QUALIFICATION_EVIDENCE" 2>/dev/null)" == "$source_commit" ]] || fail 'resume requires commit-exact pod evidence'
  for path in "$candidate" "$output"; do [[ ! -e "$path" && ! -L "$path" ]] || fail "output already exists: $path"; done
else
  for path in "$build" "$candidate" "$output"; do [[ ! -e "$path" && ! -L "$path" ]] || fail "output already exists: $path"; done
fi
for device in /dev/kvm /dev/net/tun; do
  [[ -c "$device" && -r "$device" && -w "$device" ]] || fail "requires accessible $device"
done
nested_file=/sys/module/kvm_amd/parameters/nested
[[ -f "$nested_file" ]] || nested_file=/sys/module/kvm_intel/parameters/nested
[[ "$(cat "$nested_file")" =~ ^(1|Y)$ ]] || fail 'installer guests require nested KVM'
: "${RELEASE_BUILDX_BUILDER:?set RELEASE_BUILDX_BUILDER to an operator-owned release builder}"
docker buildx inspect "$RELEASE_BUILDX_BUILDER" >/dev/null
virsh -c qemu:///system uri >/dev/null
domains="$(virsh -c qemu:///system list --all --name)"
! grep -q '^sbq-' <<<"$domains" || fail 'libvirt has sbq- domains; wait for their owner'
guest_memory="${QUALIFY_GUEST_MEMORY_MIB:-16384}"
[[ "$guest_memory" =~ ^[1-9][0-9]*$ ]] && ((guest_memory >= 8192 && guest_memory <= 1048576)) || fail 'QUALIFY_GUEST_MEMORY_MIB must be 8192..1048576'
available_mib="$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo)"
((available_mib >= guest_memory+16384)) || fail 'insufficient memory for qualification and an installer guest'
# Conservative reserves for OCI builds/candidates and three disposable guests.
for path in "$RELEASE_OUTPUT_ROOT" "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT"; do
  available_kib="$(df -Pk "$path" | awk 'NR==2 {print $4}')"
  ((available_kib >= 200*1024*1024)) || fail "less than 200 GiB free at $path"
done
export QUALIFY_ENV_FILE="$config"
scripts/qualify.sh --tier "$tier" --preflight
mkdir -p .tmp/release
exec 8>.tmp/release/checkout.lock
flock -n 8 || fail 'another release owns this checkout'
git show-ref --verify --quiet "refs/tags/$tag" || git tag "$tag" "$source_commit"
run="$(date -u +%Y%m%dT%H%M%S)-$$"
directory="$repo_root/.tmp/release/$run"
mkdir -m 700 "$directory"
echo "Release run: $run ($source_commit); logs: $directory"
! $resume || echo "Resuming from retained build $build: requalifying gates only"
started=$SECONDS
jobs=()
stage() {
  local name="$1" start=$SECONDS status=0; shift
  "$@" >"$directory/$name.log" 2>&1 || status=$?
  printf '%s\t%s\t%s\n' "$name" "$status" "$((SECONDS-start))" >"$directory/$name.status"
  return "$status"
}
finish() {
  local status=$? name code elapsed result
  trap - EXIT
  for job in "${jobs[@]}"; do wait "$job" || status=1; done
  {
    printf '| Stage | Result | Wall clock |\n|---|---|---|\n'
    for file in "$directory"/*.status; do
      IFS=$'\t' read -r name code elapsed <"$file"
      if ((code == 0)); then result=PASS; else result="FAIL ($code)"; status=1; fi
      printf '| %s | %s | %sm %ss |\n' "$name" "$result" "$((elapsed/60))" "$((elapsed%60))"
    done
    elapsed=$((SECONDS-started))
    printf '| Total | %s | %sm %ss |\n' "$([[ $status == 0 ]] && echo PASS || echo FAIL)" "$((elapsed/60))" "$((elapsed%60))"
  } | tee "$directory/timing.md"
  echo "$status" >"$directory/result"
  exit "$status"
}
trap finish EXIT
if $resume; then
  stage qualification /usr/bin/just qualify --tier "$tier" --only gates || fail "gate requalification failed; inspect $directory"
else
  stage qualification /usr/bin/just qualify --tier "$tier" & jobs+=("$!")
  stage build env BUILDX_BUILDER="$RELEASE_BUILDX_BUILDER" scripts/release-stage.sh --build-only "$version" "$build" & jobs+=("$!")
  status=0
  for job in "${jobs[@]}"; do wait "$job" || status=1; done
  jobs=()
  ((status == 0)) || fail "qualification/build failed; inspect $directory"
fi
stage candidate scripts/release-stage.sh --candidate --from-build "$build" "$version" "$candidate"
export SECONDBOX_REQUIRE_QUALIFIED_INSTALLER=1
export SECONDBOX_INSTALLER_RELEASE_DIRECTORY="$candidate"
stage installer /usr/bin/just test-installer-qualified "${installer_flags[@]}"
stage stage scripts/release-stage.sh --from-build "$build" "$version" "$output"
[[ "$(git rev-parse HEAD)" == "$source_commit" && -z "$(git status --porcelain --untracked-files=all)" ]] || fail 'source changed during release'
printf 'Staged release: %s\nPublish explicitly:\n' "$output"
printf 'git push origin refs/tags/%q\n' "$tag"
printf '/usr/bin/just release-upload %q %q\n' "$version" "$output"
if git cat-file -e "refs/tags/$tag:docs/releases/$tag.md" 2>/dev/null; then
  printf '# Release notes: docs/releases/%s.md (read from the tag)\n' "$tag"
fi

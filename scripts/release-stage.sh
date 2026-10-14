#!/usr/bin/env bash
set -euo pipefail

# A release candidate is a byte-reproducible set of files. Shell collation
# decides the order of SHA256SUMS, the candidate allowlist, and every archived
# file list, so the operator's locale would otherwise change the staged bytes and
# disagree with the byte-ordered comparisons the release tool performs.
export LC_ALL=C

usage() {
	echo "usage: scripts/release-stage.sh [--test-mode] [--candidate] {[--build-only] --shared-from SHARED_DIR | --from-build DIR} VERSION OUTPUT_DIR" >&2
  exit 2
}

test_mode=false
candidate_mode=false
build_only=false
from_build=''
shared_from=''
image_platforms="${RELEASE_IMAGE_PLATFORMS:-linux/amd64}"
# One staging produces the artifact set of one guest architecture: its Runner,
# microVM artifacts and standard Profiles. gVisor ships for amd64 guests only.
# A release publishes the amd64 set and an arm64 set under one tag. Both copy
# the architecture-neutral files that scripts/release-build-shared.sh built once
# for the same commit (--shared-from); the arm64 set suffixes its own files.
guest_architecture="${RELEASE_GUEST_ARCHITECTURE:-amd64}"
case "$guest_architecture" in
  amd64) [[ "$image_platforms" == linux/amd64 || "$image_platforms" == linux/amd64,linux/arm64 ]] || { echo 'amd64 release image platforms must be linux/amd64 or linux/amd64,linux/arm64' >&2; exit 1; } ;;
  arm64) [[ "$image_platforms" == linux/arm64 ]] || { echo 'arm64 release image platforms must be linux/arm64' >&2; exit 1; } ;;
  *) echo 'RELEASE_GUEST_ARCHITECTURE must be amd64 or arm64' >&2; exit 1 ;;
esac
guest_platform="linux/$guest_architecture"
asset_suffix=''
[[ "$guest_architecture" == amd64 ]] || asset_suffix="-$guest_architecture"
with_gvisor=false
[[ "$guest_architecture" != amd64 ]] || with_gvisor=true
host_architecture="$(go env GOHOSTARCH)"
full=false
[[ "$image_platforms" != linux/amd64,linux/arm64 ]] || full=true
while [[ "${1:-}" == --* ]]; do
	case "$1" in
		--test-mode) test_mode=true ;;
		--candidate) candidate_mode=true ;;
    --build-only) build_only=true ;;
    --from-build) [[ $# -ge 2 ]] || usage; from_build="$2"; shift ;;
    --shared-from) [[ $# -ge 2 ]] || usage; shared_from="$2"; shift ;;
		*) usage ;;
	esac
	shift
done
[[ "$#" -eq 2 ]] || usage
! $build_only || ! $candidate_mode || usage
# A retained build already holds its shared files; every other staging copies them.
if [[ -n "$from_build" ]]; then
  [[ -z "$shared_from" ]] && ! $build_only || usage
else
  [[ -n "$shared_from" ]] || { echo 'release staging requires --shared-from with the shared files built by scripts/release-build-shared.sh for the same commit' >&2; exit 1; }
fi
# Installer candidates and retained builds exist for the amd64 release flow only.
if [[ "$guest_architecture" != amd64 ]]; then
  ! $candidate_mode && ! $build_only && [[ -z "$from_build" ]] || { echo "an $guest_architecture artifact set is staged final in one pass" >&2; exit 1; }
fi
version="$1"
output_dir="$2"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
qualification_evidence_schema="secondbox.release/qualification-evidence/v2"
qualification_evidence_source="$repo_root/.tmp/scenario-qualification-evidence.json"
qualification_evidence_name="secondbox-${version}${asset_suffix}-qualification-evidence.json"
artifact_manifest_name="secondbox-${version}${asset_suffix}-artifact-manifest.json"
checksums_name="SHA256SUMS${asset_suffix}"
allowlist_name="candidate-allowlist${asset_suffix}.json"
control_plane_oci="control-plane${asset_suffix}.oci"
runner_oci="runner${asset_suffix}.oci"
installer_tools_oci="installer-tools${asset_suffix}.oci"
microvm_oci="microvm-artifacts${asset_suffix}.oci"
installer_qualification_evidence_schema="secondbox.release/installer-qualification-evidence/v2"
installer_qualification_evidence_source="$repo_root/.tmp/installer-qualification-evidence.json"
installer_qualification_evidence_name="secondbox-${version}-installer-qualification-evidence.json"

validate_qualification_evidence() {
  local evidence="$1"
  local evidence_commit
  local evidence_dirty

  [[ -f "$evidence" && ! -L "$evidence" ]] || {
    echo "release staging requires qualification evidence at $qualification_evidence_source; run just test-scenario on the qualified host" >&2
    exit 1
  }
  jq empty "$evidence" >/dev/null 2>&1 || {
    echo "release qualification evidence is malformed; rerun just test-scenario on a clean checkout" >&2
    exit 1
  }
  evidence_commit="$(jq -er '.sourceCommit | select(type == "string")' "$evidence" 2>/dev/null)" || {
    echo "release qualification evidence lacks sourceCommit; rerun just test-scenario on a clean checkout" >&2
    exit 1
  }
  [[ "$evidence_commit" == "$source_commit" ]] || {
    echo "release qualification evidence source commit $evidence_commit does not match staged source commit $source_commit; rerun just test-scenario at $source_commit" >&2
    exit 1
  }
  evidence_dirty="$(jq -r 'if (.repositoryDirty | type) == "boolean" then .repositoryDirty else error("repositoryDirty must be boolean") end' "$evidence" 2>/dev/null)" || {
    echo "release qualification evidence lacks repositoryDirty state; rerun just test-scenario on a clean checkout" >&2
    exit 1
  }
  [[ "$evidence_dirty" == "false" ]] || {
    echo "release qualification evidence was produced from a dirty repository; clean the checkout and rerun just test-scenario" >&2
    exit 1
  }
  jq -e --arg schema "$qualification_evidence_schema" --arg platform "linux-$guest_architecture" '
    .schemaVersion == $schema and
    .suite == "test-scenario" and
    (.passCount | type == "number") and .passCount > 0 and .passCount == (.passCount | floor) and
    (.wallClockSeconds | type == "number") and .wallClockSeconds >= 0 and .wallClockSeconds == (.wallClockSeconds | floor) and
    .host.platform == $platform and
    .host.kvm == {path:"/dev/kvm",present:true,readable:true,writable:true} and
    .host.tun == {path:"/dev/net/tun",present:true,readable:true,writable:true} and
    (.host.workspaceFilesystem.mount | type == "string") and (.host.workspaceFilesystem.mount | length) > 0 and
    (.host.workspaceFilesystem.type == "xfs" or .host.workspaceFilesystem.type == "btrfs") and
    (.qualifiedAt | type == "string") and
    (.qualifiedAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
  ' "$evidence" >/dev/null || {
    echo "release qualification evidence does not describe a complete qualified scenario run; rerun just test-scenario on a clean checkout" >&2
    exit 1
  }
  jq -r '"release qualification skipped groups: " + ((.skipped // []) | tojson)' "$evidence"
}

# gVisor host evidence is mandatory; full releases also bind no-KVM pod evidence.
validate_gvisor_qualification_evidence() {
  local evidence="$1" suite="$2"
  [[ -f "$evidence" && ! -L "$evidence" ]] || {
    echo "release staging requires gVisor qualification evidence at $evidence; run just $suite on its qualified host and copy the evidence here" >&2
    exit 1
  }
  jq -e --arg schema "$qualification_evidence_schema" --arg commit "$source_commit" --arg suite "$suite" '
    .schemaVersion == $schema and .sourceCommit == $commit and .repositoryDirty == false and
    .suite == $suite and .backend == "gvisor" and
    (.passCount | type == "number") and .passCount > 0 and .passCount == (.passCount | floor) and
    (.wallClockSeconds | type == "number") and .wallClockSeconds >= 0 and .wallClockSeconds == (.wallClockSeconds | floor) and
    .host.platform == "linux-amd64" and
     .host.kvm.required == false and (.host.kvm.present | type == "boolean") and
    ($suite != "test-scenario-gvisor-pod" or .host.kvm.present == false) and
    (.host.workspaceFilesystem.mount | type == "string") and (.host.workspaceFilesystem.mount | length) > 0 and
    (.host.workspaceFilesystem.type == "xfs" or .host.workspaceFilesystem.type == "btrfs") and
    (.qualifiedAt | type == "string") and
    (.qualifiedAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
  ' "$evidence" >/dev/null || {
    echo "release gVisor qualification evidence at $evidence does not describe a complete $suite run at $source_commit with the required host facts" >&2
    exit 1
  }
  jq -r '"release \(.suite) skipped groups: " + ((.skipped // []) | tojson)' "$evidence"
}

validate_installer_qualification_evidence() {
  local evidence="$1"
  [[ -f "$evidence" && ! -L "$evidence" ]] || {
    echo "release staging requires installer qualification evidence at $installer_qualification_evidence_source; run just test-installer-qualified on the qualified host" >&2
    exit 1
  }
  jq -e --arg schema "$installer_qualification_evidence_schema" --arg commit "$source_commit" --arg platform linux-amd64 '
    .schemaVersion == $schema and .sourceCommit == $commit and .repositoryDirty == false and
    .suite == "test-installer-qualified" and
    (.passCount | type == "number") and .passCount > 0 and .passCount == (.passCount | floor) and
    (.wallClockSeconds | type == "number") and .wallClockSeconds >= 0 and .wallClockSeconds == (.wallClockSeconds | floor) and
    .host.platform == $platform and
    .host.kvm == {path:"/dev/kvm",present:true,readable:true,writable:true} and
    .host.tun == {path:"/dev/net/tun",present:true,readable:true,writable:true} and
    (.host.workspaceFilesystem.mount | type == "string") and (.host.workspaceFilesystem.mount | length) > 0 and
    (.host.workspaceFilesystem.type == "xfs" or .host.workspaceFilesystem.type == "btrfs") and
    (.releaseManifestDigest | test("^sha256:[0-9a-f]{64}$")) and
    (.filesystemIdentity | type == "string") and (.filesystemIdentity | length) > 0 and
    .rebootPassed == true and
    (.qualifiedAt | type == "string") and
    (.qualifiedAt | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
  ' "$evidence" >/dev/null || {
    echo "release installer qualification evidence does not describe a complete clean qualified run; rerun just test-installer-qualified" >&2
    exit 1
  }
}

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]] || {
  echo "release version must be SemVer without a v prefix or build metadata" >&2
  exit 1
}
if $test_mode; then
  source_commit="$("$repo_root/scripts/release-source-commit.sh" --test-mode)"
else
  source_commit="$("$repo_root/scripts/release-source-commit.sh")"
fi
if ! $test_mode; then
  if ! $build_only; then validate_qualification_evidence "$qualification_evidence_source"; fi
  [[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]] || {
    echo "release staging requires a clean repository" >&2
    exit 1
  }
  tag_commit="$(git -C "$repo_root" rev-parse "refs/tags/v${version}^{commit}" 2>/dev/null)" || {
    echo "release staging requires immutable tag v${version}" >&2
    exit 1
  }
  [[ "$tag_commit" == "$source_commit" ]] || {
    echo "release tag v${version} does not identify HEAD ${source_commit}" >&2
    exit 1
  }
fi

[[ ! -e "$output_dir" ]] || { echo "release output already exists: $output_dir" >&2; exit 1; }
mkdir -p "$output_dir"
output_dir="$(cd "$output_dir" && pwd)"
mkdir -p "$repo_root/.tmp"
temporary="$(mktemp -d "$repo_root/.tmp/release-stage.XXXXXX")"
cleanup() { rm -rf "$temporary"; }
trap cleanup EXIT

if [[ -n "$from_build" ]]; then
  [[ -d "$from_build" && ! -L "$from_build" ]] || { echo 'invalid release build directory' >&2; exit 1; }
  from_build="$(cd "$from_build" && pwd)"
  [[ -z "$(find "$from_build" -mindepth 1 -maxdepth 1 ! -type f -print -quit)" ]] || { echo 'release build contains a non-regular entry' >&2; exit 1; }
  # Authenticate the complete intermediate file set before binding any evidence.
  (cd "$from_build" && sha256sum --check .release-build-sums >/dev/null)
  diff -u <(cd "$from_build" && find . -maxdepth 1 -type f ! -name .release-build-sums -printf '%f\n' | sort) \
    <(sed 's/^[a-f0-9]*  //' "$from_build/.release-build-sums" | sort)
  jq -e --arg version "$version" --arg commit "$source_commit" '.version == $version and .sourceCommit == $commit' "$from_build/.release-build.json" >/dev/null
  built_platforms="$(jq -er '.imagePlatforms | join(",")' "$from_build/.release-build.json")"
  [[ "$built_platforms" == "$image_platforms" ]] || { echo 'release build platforms differ from requested platforms' >&2; exit 1; }
  cp -a -- "$from_build/." "$output_dir/"
  mv "$output_dir/.release-build.json" "$temporary/candidate-input.json"
  rm -- "$output_dir/.release-build-sums"
else
  [[ -d "$shared_from" && ! -L "$shared_from" ]] || { echo 'invalid shared release directory' >&2; exit 1; }
  shared_from="$(cd "$shared_from" && pwd)"
  # The tool authenticates the shared files against their record, this
  # version and this source commit, and names the files to stage byte for byte.
  shared_files="$(go -C "$repo_root" run ./cmd/secondbox-release-tool verify-shared-artifacts "$shared_from" "$version" "$source_commit")"
  while IFS= read -r name; do cp -p -- "$shared_from/$name" "$output_dir/$name"; done <<<"$shared_files"
  postgres_image="$(jq -er '.postgresImage' "$shared_from/shared-artifacts.json")"
openapi_name="secondbox-${version}-openapi.json"
ldflags="-s -w -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.Version=${version} -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.SourceCommit=${source_commit}"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go -C "$repo_root" build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$temporary/secondboxd_linux_${arch}" ./cmd/secondboxd
done
CGO_ENABLED=0 GOOS=linux GOARCH="$guest_architecture" go -C "$repo_root/runner" build -trimpath -buildvcs=false -ldflags "-s -w -X main.releaseVersion=${version} -X main.sourceCommit=${source_commit}" -o "$temporary/secondbox-runner" ./cmd/secondbox-runner

verify_binary_identity() {
  local label="$1"
  shift
  "$@" | jq -e --arg version "$version" --arg commit "$source_commit" '.version == $version and .sourceCommit == $commit' >/dev/null || {
    echo "$label binary release identity mismatch" >&2
    exit 1
  }
}
verify_binary_identity secondbox "$output_dir/secondbox_${version}_linux_${host_architecture}" version
verify_binary_identity secondbox-deploy "$output_dir/secondbox-deploy_${version}_linux_${host_architecture}" version
verify_binary_identity control-plane "$temporary/secondboxd_linux_${host_architecture}" --version
if [[ "$guest_architecture" == "$host_architecture" ]]; then
  verify_binary_identity Runner "$temporary/secondbox-runner" --version
fi
for binary in "$output_dir"/secondbox*_${version}_* "$temporary/secondboxd_linux_amd64" "$temporary/secondboxd_linux_arm64" "$temporary/secondbox-runner"; do
  if ! strings "$binary" | grep -Fx -- "$version" >/dev/null &&
     ! strings "$binary" | grep -Fx -- "_${version}" >/dev/null; then
    echo "binary lacks release version: $binary" >&2
    exit 1
  fi
  strings "$binary" | grep -Fx -- "$source_commit" >/dev/null || { echo "binary lacks source commit: $binary" >&2; exit 1; }
done

: "${SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR:?release staging requires SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR}"
microvm_source="$SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR"
microvm_files=(SHA256SUMS kernel kernel-provenance.json manifest.json rootfs-debian-license-inventory.json rootfs-debian-packages.lock rootfs-python-license-inventory.json rootfs-python.freeze rootfs-source-manifest.json rootfs.ext4 runtime-manifest.json secondbox-rootfs-contract.json shared.img toolchain-manifest.json)
mapfile -t actual_microvm_files < <(find "$microvm_source" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)
mapfile -t expected_microvm_files < <(printf '%s\n' "${microvm_files[@]}" | sort)
[[ "${actual_microvm_files[*]}" == "${expected_microvm_files[*]}" ]] || { echo "microVM release bundle differs from fixed allowlist" >&2; exit 1; }
if $test_mode; then
  (cd "$microvm_source" && sha256sum -c SHA256SUMS >/dev/null)
else
  "$repo_root/runner/scripts/microvm-image/verify.sh" "$microvm_source"
fi
[[ "$(jq -er '.architecture' "$microvm_source/manifest.json")" == "$guest_architecture" ]] || { echo "microVM release bundle architecture differs from the $guest_architecture release guest architecture" >&2; exit 1; }
microvm_manifest_digest="sha256:$(sha256sum "$microvm_source/manifest.json" | awk '{print $1}')"
microvm_runtime_bundle="$(jq -ce '.runtimeBundle | {artifactId,manifestDigest,mandatoryGuestFeatures}' "$microvm_source/manifest.json")"
microvm_toolchain_bundle="$(jq -ce '.toolchainBundle | {artifactId,manifestDigest,mandatoryGuestFeatures}' "$microvm_source/manifest.json")"
microvm_runtime_digest="$(jq -er '.manifestDigest' <<<"$microvm_runtime_bundle")"
microvm_toolchain_digest="$(jq -er '.manifestDigest' <<<"$microvm_toolchain_bundle")"
[[ "$microvm_runtime_digest" != "$microvm_toolchain_digest" && "$microvm_runtime_digest" != "$microvm_manifest_digest" && "$microvm_toolchain_digest" != "$microvm_manifest_digest" ]] || {
  echo "microVM bundle manifest must bind distinct runtime and toolchain component digests" >&2
  exit 1
}

public_contract_digest="sha256:$(sha256sum "$output_dir/$openapi_name" | awk '{print $1}')"
if $test_mode; then
  control_plane_digest="sha256:$(for arch in amd64 arm64; do sha256sum "$temporary/secondboxd_linux_${arch}" | awk '{print $1}'; done | sha256sum | awk '{print $1}')"
	runner_digest="sha256:$(sha256sum "$temporary/secondbox-runner" | awk '{print $1}')"
	installer_tools_digest="sha256:$(sha256sum "$repo_root/deploy/installer-tools.Dockerfile" | awk '{print $1}')"
  microvm_image_digest="sha256:$(printf '%s\n' "${microvm_files[@]}" "$microvm_manifest_digest" | sha256sum | awk '{print $1}')"
  if $with_gvisor; then
    gvisor_runner_digest="sha256:$(sha256sum "$repo_root/runner/Dockerfile.gvisor" | awk '{print $1}')"
    gvisor_image_digest="sha256:$(sha256sum "$repo_root/runner/deploy/gvisor-artifact-transport.Dockerfile" | awk '{print $1}')"
    gvisor_runsc_release="$(sed -n 's/^readonly RUNSC_RELEASE="\([0-9.]*\)"$/\1/p' "$repo_root/runner/scripts/fetch-runsc.sh")"
    gvisor_flat_root_digest="sha256:$(printf 'synthetic release-stage gVisor flat root' | sha256sum | awk '{print $1}')"
    jq -cn --arg runsc "$gvisor_runsc_release" --arg flatRoot "$gvisor_flat_root_digest" --arg version "$version" '{schemaVersion:"secondbox.runner/backend-materialization/v1",key:{backendKind:"gvisor",guestArchitecture:"amd64",runtimeManifestDigest:("sha256:"+("0"*64)),toolchainManifestDigest:("sha256:"+("1"*64))},sourceOciManifestDigest:("sha256:"+("2"*64)),flatRootDigest:$flatRoot,launchArtifacts:[{id:"guest-agent",sha256:("sha256:"+("3"*64))},{id:"runsc",sha256:("sha256:"+("4"*64))}],agentProtocolGeneration:1,agentFeatures:["exec-streaming","file-streaming","port-proxy","pty"],backendBuildId:("secondbox-gvisor-release-"+$version),helperBuildId:("runsc-release-"+$runsc)}' >"$output_dir/secondbox-${version}-gvisor-materialization.json"
    gvisor_materialization_digest="$(go -C "$repo_root/runner" run ./cmd/secondbox-materialization-digest "$output_dir/secondbox-${version}-gvisor-materialization.json")"
    jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$gvisor_runner_digest" --arg contract "$public_contract_digest" '{image:"runner-gvisor",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64"],publicContractDigest:$contract}' >"$output_dir/runner-gvisor.oci.json"
    jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$gvisor_image_digest" --arg materialization "$gvisor_materialization_digest" '{image:"gvisor-artifacts",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64"],materializationDigest:$materialization}' >"$output_dir/gvisor-artifacts.oci.json"
  else
    gvisor_runner_digest='' gvisor_image_digest='' gvisor_materialization_digest='' gvisor_flat_root_digest='' gvisor_runsc_release=''
  fi
  jq -n --arg version "$version" --arg commit "$source_commit" --argjson platforms "$(jq -cn --arg platforms "$image_platforms" '$platforms|split(",")')" --arg digest "$control_plane_digest" --arg contract "$public_contract_digest" '{image:"control-plane",version:$version,sourceCommit:$commit,digest:$digest,platforms:$platforms,publicContractDigest:$contract}' >"$output_dir/$control_plane_oci.json"
	jq -n --arg platform "$guest_platform" --arg version "$version" --arg commit "$source_commit" --arg digest "$runner_digest" --arg contract "$public_contract_digest" '{image:"runner",version:$version,sourceCommit:$commit,digest:$digest,platforms:[$platform],publicContractDigest:$contract}' >"$output_dir/$runner_oci.json"
	jq -n --arg platform "$guest_platform" --arg version "$version" --arg commit "$source_commit" --arg digest "$installer_tools_digest" '{image:"installer-tools",version:$version,sourceCommit:$commit,digest:$digest,platforms:[$platform]}' >"$output_dir/$installer_tools_oci.json"
  jq -n --arg platform "$guest_platform" --arg version "$version" --arg commit "$source_commit" --arg digest "$microvm_image_digest" --arg manifest "$microvm_manifest_digest" '{image:"microvm-artifacts",version:$version,sourceCommit:$commit,digest:$digest,platforms:[$platform],bundleManifestDigest:$manifest}' >"$output_dir/$microvm_oci.json"
else
  docker buildx build --platform "$image_platforms" --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "PUBLIC_CONTRACT_DIGEST=$public_contract_digest" --output "type=oci,dest=$output_dir/$control_plane_oci.tar" --metadata-file "$output_dir/$control_plane_oci.json" "$repo_root"
	docker buildx build --platform "$guest_platform" --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "PUBLIC_CONTRACT_DIGEST=$public_contract_digest" --file "$repo_root/runner/Dockerfile" --output "type=oci,dest=$output_dir/$runner_oci.tar" --metadata-file "$output_dir/$runner_oci.json" "$repo_root"
	docker buildx build --platform "$guest_platform" --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --file "$repo_root/deploy/installer-tools.Dockerfile" --output "type=oci,dest=$output_dir/$installer_tools_oci.tar" --metadata-file "$output_dir/$installer_tools_oci.json" "$repo_root"
  docker buildx build --platform "$guest_platform" --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "BUNDLE_MANIFEST_DIGEST=$microvm_manifest_digest" --file "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile" --output "type=oci,dest=$output_dir/$microvm_oci.tar" --metadata-file "$output_dir/$microvm_oci.json" "$microvm_source"
  control_plane_digest="$(jq -er '."containerimage.digest"' "$output_dir/$control_plane_oci.json")"
	runner_digest="$(jq -er '."containerimage.digest"' "$output_dir/$runner_oci.json")"
	installer_tools_digest="$(jq -er '."containerimage.digest"' "$output_dir/$installer_tools_oci.json")"
  microvm_image_digest="$(jq -er '."containerimage.digest"' "$output_dir/$microvm_oci.json")"
  if $with_gvisor; then
    # The gVisor runner and its artifact transport are built from the
    # repository and the pinned bases alone; the transport's metadata target
    # exports the materialization the manifest records and the release ships.
    docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "PUBLIC_CONTRACT_DIGEST=$public_contract_digest" --file "$repo_root/runner/Dockerfile.gvisor" --output "type=oci,dest=$output_dir/runner-gvisor.oci.tar" --metadata-file "$output_dir/runner-gvisor.oci.json" "$repo_root"
    docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --file "$repo_root/runner/deploy/gvisor-artifact-transport.Dockerfile" --target metadata --output "type=local,dest=$temporary/gvisor-metadata" "$repo_root"
    docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --file "$repo_root/runner/deploy/gvisor-artifact-transport.Dockerfile" --output "type=oci,dest=$output_dir/gvisor-artifacts.oci.tar" --metadata-file "$output_dir/gvisor-artifacts.oci.json" "$repo_root"
    gvisor_runner_digest="$(jq -er '."containerimage.digest"' "$output_dir/runner-gvisor.oci.json")"
    gvisor_image_digest="$(jq -er '."containerimage.digest"' "$output_dir/gvisor-artifacts.oci.json")"
    cp "$temporary/gvisor-metadata/materialization.json" "$output_dir/secondbox-${version}-gvisor-materialization.json"
    chmod 0644 "$output_dir/secondbox-${version}-gvisor-materialization.json"
    gvisor_materialization_digest="$(jq -er '.materializationDigest' "$temporary/gvisor-metadata/identity.json")"
    gvisor_flat_root_digest="$(jq -er '.flatRootDigest' "$temporary/gvisor-metadata/identity.json")"
    gvisor_runsc_release="$(jq -er '.runscRelease' "$temporary/gvisor-metadata/identity.json")"
    [[ "$(go -C "$repo_root/runner" run ./cmd/secondbox-materialization-digest "$output_dir/secondbox-${version}-gvisor-materialization.json")" == "$gvisor_materialization_digest" ]] || { echo "gVisor materialization digest differs from the transport identity" >&2; exit 1; }
  else
    gvisor_runner_digest='' gvisor_image_digest='' gvisor_materialization_digest='' gvisor_flat_root_digest='' gvisor_runsc_release=''
  fi
fi

go -C "$repo_root" run ./cmd/secondbox-release-tool standard-documents "$guest_architecture" "$output_dir"

jq -n --argjson imagePlatforms "$(jq -cn --arg platforms "$image_platforms" '$platforms|split(",")')" --argjson candidate "$candidate_mode" --arg version "$version" --arg commit "$source_commit" --arg control "$control_plane_digest" --arg runner "$runner_digest" --arg installerTools "$installer_tools_digest" --arg postgresImage "$postgres_image" --arg microImage "$microvm_image_digest" --arg microManifest "$microvm_manifest_digest" --argjson runtime "$microvm_runtime_bundle" --argjson toolchain "$microvm_toolchain_bundle" --arg gvisorRunner "$gvisor_runner_digest" --arg gvisorImage "$gvisor_image_digest" --arg gvisorMaterialization "$gvisor_materialization_digest" --arg gvisorFlatRoot "$gvisor_flat_root_digest" --arg gvisorRunsc "$gvisor_runsc_release" --arg guestArchitecture "$guest_architecture" '{imagePlatforms:$imagePlatforms,guestArchitecture:$guestArchitecture,candidate:$candidate,version:$version,sourceCommit:$commit,controlPlaneDigest:$control,runnerDigest:$runner,installerToolsDigest:$installerTools,postgresImage:$postgresImage,microvmImageDigest:$microImage,microvmManifestDigest:$microManifest,microvmRuntimeBundle:$runtime,microvmToolchainBundle:$toolchain,gvisorRunnerDigest:$gvisorRunner,gvisorImageDigest:$gvisorImage,gvisorMaterializationDigest:$gvisorMaterialization,gvisorFlatRootDigest:$gvisorFlatRoot,gvisorRunscRelease:$gvisorRunsc}' >"$temporary/candidate-input.json"
fi
if $build_only; then
  cp "$temporary/candidate-input.json" "$output_dir/.release-build.json"
  (cd "$output_dir" && find . -maxdepth 1 -type f ! -name .release-build-sums -printf '%f\n' | sort | xargs sha256sum >.release-build-sums)
  echo "Unbound release build: $output_dir"
  exit 0
fi
if $test_mode; then
	jq -n \
    --arg schemaVersion "$qualification_evidence_schema" \
    --arg sourceCommit "$source_commit" \
    --arg guestArchitecture "$guest_architecture" \
    '{
      schemaVersion: $schemaVersion,
      sourceCommit: $sourceCommit,
      repositoryDirty: false,
      suite: "test-scenario",
      passCount: 16,
      wallClockSeconds: 1,
      host: {
        platform: ("linux-" + $guestArchitecture),
        kvm: {path: "/dev/kvm", present: true, readable: true, writable: true},
        tun: {path: "/dev/net/tun", present: true, readable: true, writable: true},
        workspaceFilesystem: {mount: "/synthetic/qualification xfs", type: "xfs"}
      },
      qualifiedAt: "1970-01-01T00:00:00Z"
    }' >"$output_dir/$qualification_evidence_name"
else
  install -m 0644 "$qualification_evidence_source" "$output_dir/$qualification_evidence_name"
fi
validate_qualification_evidence "$output_dir/$qualification_evidence_name"

if $with_gvisor; then
  gvisor_qualification_evidence_name="secondbox-${version}-gvisor-qualification-evidence.json"
  gvisor_pod_qualification_evidence_name="secondbox-${version}-gvisor-pod-qualification-evidence.json"
  if $test_mode; then
    suites=(test-scenario-gvisor)
    if $full; then suites+=(test-scenario-gvisor-pod); fi
    for suite in "${suites[@]}"; do
      target="$output_dir/$gvisor_qualification_evidence_name"
      [[ "$suite" == test-scenario-gvisor ]] || target="$output_dir/$gvisor_pod_qualification_evidence_name"
      jq -n --arg schemaVersion "$qualification_evidence_schema" --arg sourceCommit "$source_commit" --arg suite "$suite" '{schemaVersion:$schemaVersion,sourceCommit:$sourceCommit,repositoryDirty:false,suite:$suite,backend:"gvisor",passCount:16,wallClockSeconds:1,host:{platform:"linux-amd64",kvm:{required:false,present:false},tun:{required:false},workspaceFilesystem:{mount:"/synthetic/qualification xfs",type:"xfs"}},qualifiedAt:"1970-01-01T00:00:00Z"}' >"$target"
    done
  else
    install -m 0644 "${SECONDBOX_GVISOR_QUALIFICATION_EVIDENCE:-$repo_root/.tmp/gvisor-linux-scenario-qualification-evidence.json}" "$output_dir/$gvisor_qualification_evidence_name"
    if $full; then install -m 0644 "${SECONDBOX_GVISOR_POD_QUALIFICATION_EVIDENCE:-$repo_root/.tmp/gvisor-pod-linux-scenario-qualification-evidence.json}" "$output_dir/$gvisor_pod_qualification_evidence_name"; fi
  fi
  validate_gvisor_qualification_evidence "$output_dir/$gvisor_qualification_evidence_name" test-scenario-gvisor
  if $full; then validate_gvisor_qualification_evidence "$output_dir/$gvisor_pod_qualification_evidence_name" test-scenario-gvisor-pod; fi
fi

# The guided installer installs amd64 releases only, so only a final amd64
# release carries installer qualification evidence.
with_installer_qualification=false
! $candidate_mode && [[ "$guest_architecture" == amd64 ]] && with_installer_qualification=true
if $test_mode && $with_installer_qualification; then
  jq -n \
    --arg schemaVersion "$installer_qualification_evidence_schema" \
    --arg sourceCommit "$source_commit" \
    --arg releaseManifestDigest "sha256:$(printf '%s' "$source_commit-installer-qualified" | sha256sum | awk '{print $1}')" \
    '{schemaVersion:$schemaVersion,sourceCommit:$sourceCommit,repositoryDirty:false,suite:"test-installer-qualified",passCount:24,wallClockSeconds:1,host:{platform:"linux-amd64",kvm:{path:"/dev/kvm",present:true,readable:true,writable:true},tun:{path:"/dev/net/tun",present:true,readable:true,writable:true},workspaceFilesystem:{mount:"/synthetic/installer xfs",type:"xfs"}},releaseManifestDigest:$releaseManifestDigest,filesystemIdentity:"8:16",rebootPassed:true,qualifiedAt:"1970-01-01T00:00:00Z"}' >"$output_dir/$installer_qualification_evidence_name"
elif $with_installer_qualification; then
	install -m 0644 "$installer_qualification_evidence_source" "$output_dir/$installer_qualification_evidence_name"
fi
if $with_installer_qualification; then
	validate_installer_qualification_evidence "$output_dir/$installer_qualification_evidence_name"
fi

jq --argjson candidate "$candidate_mode" '.candidate = $candidate' "$temporary/candidate-input.json" >"$temporary/bound-input.json"
mv "$temporary/bound-input.json" "$temporary/candidate-input.json"
go -C "$repo_root" run ./cmd/secondbox-release-tool manifest "$temporary/candidate-input.json" "$output_dir"
artifact_manifest="$output_dir/$artifact_manifest_name"
installer_qualification_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$artifact_manifest")"
if $test_mode && $with_installer_qualification; then
  jq --arg digest "$installer_qualification_subject" '.releaseManifestDigest = $digest' "$output_dir/$installer_qualification_evidence_name" >"$temporary/installer-qualification-evidence.json"
  mv "$temporary/installer-qualification-evidence.json" "$output_dir/$installer_qualification_evidence_name"
  go -C "$repo_root" run ./cmd/secondbox-release-tool manifest "$temporary/candidate-input.json" "$output_dir"
  installer_qualification_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$artifact_manifest")"
fi
if $with_installer_qualification; then
	[[ "$(jq -er '.releaseManifestDigest' "$output_dir/$installer_qualification_evidence_name")" == "$installer_qualification_subject" ]] || {
		echo 'release installer qualification evidence was produced for different release bytes' >&2
		exit 1
	}
fi

(
  cd "$output_dir"
  find . -maxdepth 1 -type f ! -name '*.oci.tar' ! -name "$checksums_name" ! -name "$allowlist_name" -printf '%f\n' | sort | xargs sha256sum >"$checksums_name"
)
mapfile -t allowlist < <({ find "$output_dir" -maxdepth 1 -type f -printf '%f\n'; printf '%s\n' "$allowlist_name"; } | sort)
printf '%s\n' "${allowlist[@]}" | jq -R . | jq -s '{schemaVersion:1,files:.}' >"$output_dir/$allowlist_name"

go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$output_dir"
source_less_consumer="$temporary/source-less-consumer"
deployment_dir="$source_less_consumer/deployment"
mkdir "$source_less_consumer"
(
  cd "$source_less_consumer"
  for backend in firecracker gvisor; do
    "$output_dir/secondbox-deploy_${version}_linux_${host_architecture}" runner-template --backend "$backend" >"runner-$backend.toml"
    grep -q "^compute_backend = '$backend'\$" "runner-$backend.toml"
    grep -q '^egress_context_config_path = ' "runner-$backend.toml"
    grep -q '^egress_contexts = \[\]$' "runner-$backend.toml"
    if grep -q 'network_policy_runner_gateways' "runner-$backend.toml"; then
      echo "SecondBox release staging: the $backend Runner template names retired network_policy_runner_gateways" >&2
      exit 1
    fi
  done
  "$output_dir/secondbox-deploy_${version}_linux_${host_architecture}" init --mode development "$deployment_dir" >/dev/null
  "$output_dir/secondbox-deploy_${version}_linux_${host_architecture}" inspect "$deployment_dir/secondbox.toml" >/dev/null
)

echo "$output_dir"

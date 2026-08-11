#!/usr/bin/env bash
set -euo pipefail

# A release candidate is a byte-reproducible set of files. Shell collation
# decides the order of SHA256SUMS, the candidate allowlist, and every archived
# file list, so the operator's locale would otherwise change the staged bytes and
# disagree with the byte-ordered comparisons the release tool performs.
export LC_ALL=C

usage() {
	echo "usage: scripts/release-stage.sh [--test-mode] [--candidate] VERSION OUTPUT_DIR" >&2
  exit 2
}

test_mode=false
candidate_mode=false
while [[ "${1:-}" == --* ]]; do
	case "$1" in
		--test-mode) test_mode=true ;;
		--candidate) candidate_mode=true ;;
		*) usage ;;
	esac
	shift
done
[[ "$#" -eq 2 ]] || usage
version="$1"
output_dir="$2"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
qualification_evidence_schema="secondbox.release/qualification-evidence/v1"
qualification_evidence_source="$repo_root/.tmp/scenario-qualification-evidence.json"
qualification_evidence_name="secondbox-${version}-qualification-evidence.json"
installer_qualification_evidence_schema="secondbox.release/installer-qualification-evidence/v1"
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
  jq -e --arg schema "$qualification_evidence_schema" '
    .schemaVersion == $schema and
    .suite == "test-scenario" and
    (.passCount | type == "number") and .passCount > 0 and .passCount == (.passCount | floor) and
    (.wallClockSeconds | type == "number") and .wallClockSeconds >= 0 and .wallClockSeconds == (.wallClockSeconds | floor) and
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
}

validate_installer_qualification_evidence() {
  local evidence="$1"
  [[ -f "$evidence" && ! -L "$evidence" ]] || {
    echo "release staging requires installer qualification evidence at $installer_qualification_evidence_source; run just test-installer-qualified on the qualified host" >&2
    exit 1
  }
  jq -e --arg schema "$installer_qualification_evidence_schema" --arg commit "$source_commit" '
    .schemaVersion == $schema and .sourceCommit == $commit and .repositoryDirty == false and
    .suite == "test-installer-qualified" and
    (.passCount | type == "number") and .passCount > 0 and .passCount == (.passCount | floor) and
    (.wallClockSeconds | type == "number") and .wallClockSeconds >= 0 and .wallClockSeconds == (.wallClockSeconds | floor) and
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
source_commit="$(git -C "$repo_root" rev-parse HEAD)"
if $test_mode; then
  source_commit="$(
    while IFS= read -r file; do
      [[ -f "$repo_root/$file" ]] || continue
      printf '%s %s\n' "$file" "$(sha256sum "$repo_root/$file" | awk '{print $1}')"
    done < <(git -C "$repo_root" ls-files --cached --others --exclude-standard | sort) |
      sha256sum | awk '{print substr($1,1,40)}')"
fi
if ! $test_mode; then
  validate_qualification_evidence "$qualification_evidence_source"
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

if $test_mode; then
	jq -n \
    --arg schemaVersion "$qualification_evidence_schema" \
    --arg sourceCommit "$source_commit" \
    '{
      schemaVersion: $schemaVersion,
      sourceCommit: $sourceCommit,
      repositoryDirty: false,
      suite: "test-scenario",
      passCount: 16,
      wallClockSeconds: 1,
      host: {
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

if $test_mode && ! $candidate_mode; then
  jq -n \
    --arg schemaVersion "$installer_qualification_evidence_schema" \
    --arg sourceCommit "$source_commit" \
    --arg releaseManifestDigest "sha256:$(printf '%s' "$source_commit-installer-qualified" | sha256sum | awk '{print $1}')" \
    '{schemaVersion:$schemaVersion,sourceCommit:$sourceCommit,repositoryDirty:false,suite:"test-installer-qualified",passCount:22,wallClockSeconds:1,host:{kvm:{path:"/dev/kvm",present:true,readable:true,writable:true},tun:{path:"/dev/net/tun",present:true,readable:true,writable:true},workspaceFilesystem:{mount:"/synthetic/installer xfs",type:"xfs"}},releaseManifestDigest:$releaseManifestDigest,filesystemIdentity:"8:16",rebootPassed:true,qualifiedAt:"1970-01-01T00:00:00Z"}' >"$output_dir/$installer_qualification_evidence_name"
elif ! $candidate_mode; then
	install -m 0644 "$installer_qualification_evidence_source" "$output_dir/$installer_qualification_evidence_name"
fi
if ! $candidate_mode; then
	validate_installer_qualification_evidence "$output_dir/$installer_qualification_evidence_name"
fi

if $test_mode; then
	postgres_image="docker.io/library/postgres@sha256:$(printf postgres | sha256sum | awk '{print $1}')"
else
	: "${SECONDBOX_RELEASE_POSTGRES_IMAGE:?release staging requires digest-pinned SECONDBOX_RELEASE_POSTGRES_IMAGE}"
	postgres_image="$SECONDBOX_RELEASE_POSTGRES_IMAGE"
fi

if ! $test_mode; then
  "$repo_root/scripts/verify-generated.sh"
fi

openapi_name="secondbox-${version}-openapi.json"
cp "$repo_root/contracts/openapi/v1/secondbox.openapi.json" "$output_dir/$openapi_name"

if $test_mode; then
  while IFS= read -r file; do [[ -f "$repo_root/$file" ]] && printf '%s\n' "$file"; done < <(git -C "$repo_root" ls-files --cached --others --exclude-standard | sort) | tar -C "$repo_root" --create --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner --files-from=- | gzip -n >"$output_dir/secondbox-${version}-go-module.tar.gz"
else
  git -C "$repo_root" archive --format=tar "$source_commit" | gzip -n >"$output_dir/secondbox-${version}-go-module.tar.gz"
fi
go -C "$repo_root" mod verify
go -C "$repo_root" list -m >/dev/null
module_smoke="$temporary/go-module"
mkdir "$module_smoke"
tar -xzf "$output_dir/secondbox-${version}-go-module.tar.gz" -C "$module_smoke"
go -C "$module_smoke" mod verify
go -C "$module_smoke" list ./sdk/go/secondboxclient >/dev/null

sdk_copy="$temporary/sdk-typescript"
cp -a "$repo_root/sdk/typescript" "$sdk_copy"
npm --prefix "$sdk_copy" version "$version" --no-git-tag-version --ignore-scripts >/dev/null
npm pack "$sdk_copy" --pack-destination "$output_dir" >/dev/null
typescript_name="secondstack-ai-secondbox-${version}.tgz"
[[ -f "$output_dir/$typescript_name" ]] || { echo "TypeScript package name is not canonical" >&2; exit 1; }
while IFS= read -r packaged; do
  case "$packaged" in
    package/package.json|package/LICENSE|package/README.md|package/dist/*.js|package/dist/*.js.map|package/dist/*.d.ts|package/dist/*.d.ts.map) ;;
    *) echo "TypeScript package contains undeclared file: $packaged" >&2; exit 1 ;;
  esac
done < <(tar -tzf "$output_dir/$typescript_name")

ldflags="-s -w -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.Version=${version} -X github.com/SecondStack-AI/SecondBox/pkg/buildinfo.SourceCommit=${source_commit}"
host_platforms=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)
for platform in "${host_platforms[@]}"; do
  os="${platform%/*}"
  arch="${platform#*/}"
  for command in secondbox secondbox-deploy; do
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go -C "$repo_root" build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$output_dir/${command}_${version}_${os}_${arch}" "./cmd/$command"
  done
done
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go -C "$repo_root" build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$temporary/secondboxd_linux_${arch}" ./cmd/secondboxd
done
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go -C "$repo_root/runner" build -trimpath -buildvcs=false -ldflags "-s -w -X main.releaseVersion=${version} -X main.sourceCommit=${source_commit}" -o "$temporary/secondbox-runner" ./cmd/secondbox-runner

verify_binary_identity() {
  local label="$1"
  shift
  "$@" | jq -e --arg version "$version" --arg commit "$source_commit" '.version == $version and .sourceCommit == $commit' >/dev/null || {
    echo "$label binary release identity mismatch" >&2
    exit 1
  }
}
verify_binary_identity secondbox "$output_dir/secondbox_${version}_linux_amd64" version
verify_binary_identity secondbox-deploy "$output_dir/secondbox-deploy_${version}_linux_amd64" version
verify_binary_identity control-plane "$temporary/secondboxd_linux_amd64" --version
verify_binary_identity Runner "$temporary/secondbox-runner" --version
for binary in "$output_dir"/secondbox*_${version}_* "$temporary/secondboxd_linux_arm64"; do
  if ! strings "$binary" | grep -Fx -- "$version" >/dev/null &&
     ! strings "$binary" | grep -Fx -- "_${version}" >/dev/null; then
    echo "binary lacks release version: $binary" >&2
    exit 1
  fi
  strings "$binary" | grep -Fx -- "$source_commit" >/dev/null || { echo "binary lacks source commit: $binary" >&2; exit 1; }
done
deploy_linux_amd64="$output_dir/secondbox-deploy_${version}_linux_amd64"
"$repo_root/scripts/generate-install-bootstrap.sh" "$version" "$(sha256sum "$deploy_linux_amd64" | awk '{print $1}')" "$output_dir/install.sh"

: "${SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR:?release staging requires SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR}"
: "${SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY:?release staging requires SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY}"
: "${SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256:?release staging requires SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256}"
microvm_source="$SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR"
microvm_key="$SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY"
microvm_fingerprint="$SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256"
microvm_files=(SHA256SUMS kernel kernel-provenance.json manifest.json manifest.sig rootfs-debian-license-inventory.json rootfs-debian-packages.lock rootfs-python-license-inventory.json rootfs-python.freeze rootfs-source-manifest.json rootfs.ext4 runtime-manifest.json secondbox-rootfs-contract.json shared.img signing.pub toolchain-manifest.json)
mapfile -t actual_microvm_files < <(find "$microvm_source" -mindepth 1 -maxdepth 1 -type f -printf '%f\n' | sort)
mapfile -t expected_microvm_files < <(printf '%s\n' "${microvm_files[@]}" | sort)
[[ "${actual_microvm_files[*]}" == "${expected_microvm_files[*]}" ]] || { echo "microVM release bundle differs from fixed allowlist" >&2; exit 1; }
actual_fingerprint="$(openssl pkey -pubin -in "$microvm_key" -outform DER 2>/dev/null | sha256sum | awk '{print $1}')"
[[ "$actual_fingerprint" == "$microvm_fingerprint" ]] || { echo "microVM release trust-anchor fingerprint mismatch" >&2; exit 1; }
if $test_mode; then
  (cd "$microvm_source" && sha256sum -c SHA256SUMS >/dev/null)
  openssl dgst -sha256 -verify "$microvm_key" -signature "$microvm_source/manifest.sig" "$microvm_source/manifest.json" >/dev/null
else
  "$repo_root/runner/scripts/microvm-image/verify.sh" "$microvm_source" "$microvm_key" "$microvm_fingerprint"
fi
microvm_manifest_digest="sha256:$(sha256sum "$microvm_source/manifest.json" | awk '{print $1}')"
microvm_runtime_bundle="$(jq -ce '.runtimeBundle | {artifactId,manifestDigest,mandatoryGuestFeatures}' "$microvm_source/manifest.json")"
microvm_toolchain_bundle="$(jq -ce '.toolchainBundle | {artifactId,manifestDigest,mandatoryGuestFeatures}' "$microvm_source/manifest.json")"
microvm_runtime_digest="$(jq -er '.manifestDigest' <<<"$microvm_runtime_bundle")"
microvm_toolchain_digest="$(jq -er '.manifestDigest' <<<"$microvm_toolchain_bundle")"
[[ "$microvm_runtime_digest" != "$microvm_toolchain_digest" && "$microvm_runtime_digest" != "$microvm_manifest_digest" && "$microvm_toolchain_digest" != "$microvm_manifest_digest" ]] || {
  echo "microVM signed manifest must bind distinct runtime and toolchain component digests" >&2
  exit 1
}

public_contract_digest="sha256:$(sha256sum "$output_dir/$openapi_name" | awk '{print $1}')"
if $test_mode; then
  control_plane_digest="sha256:$(for arch in amd64 arm64; do sha256sum "$temporary/secondboxd_linux_${arch}" | awk '{print $1}'; done | sha256sum | awk '{print $1}')"
	runner_digest="sha256:$(sha256sum "$temporary/secondbox-runner" | awk '{print $1}')"
	installer_tools_digest="sha256:$(sha256sum "$repo_root/deploy/installer-tools.Dockerfile" | awk '{print $1}')"
  microvm_image_digest="sha256:$(printf '%s\n' "${microvm_files[@]}" "$microvm_manifest_digest" | sha256sum | awk '{print $1}')"
  jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$control_plane_digest" --arg contract "$public_contract_digest" '{image:"control-plane",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64","linux/arm64"],publicContractDigest:$contract}' >"$output_dir/control-plane.oci.json"
	jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$runner_digest" --arg contract "$public_contract_digest" '{image:"runner",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64"],publicContractDigest:$contract}' >"$output_dir/runner.oci.json"
	jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$installer_tools_digest" '{image:"installer-tools",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64"]}' >"$output_dir/installer-tools.oci.json"
  jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$microvm_image_digest" --arg manifest "$microvm_manifest_digest" --arg fingerprint "$microvm_fingerprint" '{image:"microvm-artifacts",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64"],signedManifestDigest:$manifest,signingKeyFingerprint:$fingerprint}' >"$output_dir/microvm-artifacts.oci.json"
else
  docker buildx build --platform linux/amd64,linux/arm64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "PUBLIC_CONTRACT_DIGEST=$public_contract_digest" --output "type=oci,dest=$output_dir/control-plane.oci.tar" --metadata-file "$output_dir/control-plane.oci.json" "$repo_root"
	docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "PUBLIC_CONTRACT_DIGEST=$public_contract_digest" --file "$repo_root/runner/Dockerfile" --output "type=oci,dest=$output_dir/runner.oci.tar" --metadata-file "$output_dir/runner.oci.json" "$repo_root"
	docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --file "$repo_root/deploy/installer-tools.Dockerfile" --output "type=oci,dest=$output_dir/installer-tools.oci.tar" --metadata-file "$output_dir/installer-tools.oci.json" "$repo_root"
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "SIGNED_MANIFEST_DIGEST=$microvm_manifest_digest" --file "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile" --output "type=oci,dest=$output_dir/microvm-artifacts.oci.tar" --metadata-file "$output_dir/microvm-artifacts.oci.json" "$microvm_source"
  control_plane_digest="$(jq -er '."containerimage.digest"' "$output_dir/control-plane.oci.json")"
	runner_digest="$(jq -er '."containerimage.digest"' "$output_dir/runner.oci.json")"
	installer_tools_digest="$(jq -er '."containerimage.digest"' "$output_dir/installer-tools.oci.json")"
  microvm_image_digest="$(jq -er '."containerimage.digest"' "$output_dir/microvm-artifacts.oci.json")"
fi

go -C "$repo_root" run ./cmd/secondbox-release-tool standard-documents "$microvm_manifest_digest" "$microvm_runtime_digest" "$microvm_toolchain_digest" "$output_dir"

jq -n --arg version "$version" --arg commit "$source_commit" --arg ts "$typescript_name" --arg go "secondbox-${version}-go-module.tar.gz" '{schemaVersion:1,version:$version,sourceCommit:$commit,typeScriptPackage:$ts,goModuleArchive:$go}' >"$output_dir/secondbox-${version}-package-metadata.json"
jq -n --arg version "$version" --arg commit "$source_commit" '{spdxVersion:"SPDX-2.3",dataLicense:"CC0-1.0",SPDXID:"SPDXRef-DOCUMENT",name:("SecondBox-"+$version),documentNamespace:("https://github.com/SecondStack-AI/SecondBox/releases/tag/v"+$version),creationInfo:{creators:["Organization: SecondStack AI"],comment:("deterministic source commit "+$commit)},packages:[{name:"SecondBox",SPDXID:"SPDXRef-Package-SecondBox",versionInfo:$version,downloadLocation:("git+https://github.com/SecondStack-AI/SecondBox.git@"+$commit),filesAnalyzed:false}]}' >"$output_dir/secondbox-${version}.spdx.json"
jq -n --argjson candidate "$candidate_mode" --arg version "$version" --arg commit "$source_commit" --arg control "$control_plane_digest" --arg runner "$runner_digest" --arg installerTools "$installer_tools_digest" --arg postgresImage "$postgres_image" --arg microImage "$microvm_image_digest" --arg microManifest "$microvm_manifest_digest" --arg fingerprint "$microvm_fingerprint" --argjson runtime "$microvm_runtime_bundle" --argjson toolchain "$microvm_toolchain_bundle" '{candidate:$candidate,version:$version,sourceCommit:$commit,controlPlaneDigest:$control,runnerDigest:$runner,installerToolsDigest:$installerTools,postgresImage:$postgresImage,microvmImageDigest:$microImage,microvmManifestDigest:$microManifest,microvmSigningKeyFingerprint:$fingerprint,microvmRuntimeBundle:$runtime,microvmToolchainBundle:$toolchain}' >"$temporary/candidate-input.json"
go -C "$repo_root" run ./cmd/secondbox-release-tool manifest "$temporary/candidate-input.json" "$output_dir"
artifact_manifest="$output_dir/secondbox-${version}-artifact-manifest.json"
installer_qualification_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$artifact_manifest")"
if $test_mode && ! $candidate_mode; then
  jq --arg digest "$installer_qualification_subject" '.releaseManifestDigest = $digest' "$output_dir/$installer_qualification_evidence_name" >"$temporary/installer-qualification-evidence.json"
  mv "$temporary/installer-qualification-evidence.json" "$output_dir/$installer_qualification_evidence_name"
  go -C "$repo_root" run ./cmd/secondbox-release-tool manifest "$temporary/candidate-input.json" "$output_dir"
  installer_qualification_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$artifact_manifest")"
fi
if ! $candidate_mode; then
	[[ "$(jq -er '.releaseManifestDigest' "$output_dir/$installer_qualification_evidence_name")" == "$installer_qualification_subject" ]] || {
		echo 'release installer qualification evidence was produced for different release bytes' >&2
		exit 1
	}
fi

(
  cd "$output_dir"
  find . -maxdepth 1 -type f ! -name '*.oci.tar' ! -name SHA256SUMS ! -name candidate-allowlist.json -printf '%f\n' | sort | xargs sha256sum >SHA256SUMS
)
mapfile -t allowlist < <({ find "$output_dir" -maxdepth 1 -type f -printf '%f\n'; printf '%s\n' candidate-allowlist.json; } | sort)
printf '%s\n' "${allowlist[@]}" | jq -R . | jq -s '{schemaVersion:1,files:.}' >"$output_dir/candidate-allowlist.json"

go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$output_dir"
node_project="$temporary/node-smoke"
mkdir "$node_project"
(cd "$node_project" && npm init --yes >/dev/null && npm install --ignore-scripts "$output_dir/$typescript_name" >/dev/null && node -e 'import("@secondstack-ai/secondbox").then(m => { if (typeof m.SecondBox !== "function") process.exit(1) })')
deployment_dir="$temporary/deployment"
"$output_dir/secondbox-deploy_${version}_linux_amd64" init --mode development "$deployment_dir" >/dev/null
"$output_dir/secondbox-deploy_${version}_linux_amd64" inspect "$deployment_dir/secondbox.toml" >/dev/null

echo "$output_dir"

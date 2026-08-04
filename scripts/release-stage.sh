#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: scripts/release-stage.sh [--test-mode] VERSION OUTPUT_DIR" >&2
  exit 2
}

test_mode=false
if [[ "${1:-}" == "--test-mode" ]]; then
  test_mode=true
  shift
fi
[[ "$#" -eq 2 ]] || usage
version="$1"
output_dir="$2"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

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

if ! $test_mode; then
  "$repo_root/scripts/verify-generated.sh"
fi

openapi_name="secondbox-${version}-openapi.json"
cp "$repo_root/contracts/openapi/v1/secondbox.openapi.json" "$output_dir/$openapi_name"
cp "$repo_root/scripts/test-source-free-release.sh" "$output_dir/secondbox-${version}-source-free-qualify"
chmod 0755 "$output_dir/secondbox-${version}-source-free-qualify"

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
  microvm_image_digest="sha256:$(printf '%s\n' "${microvm_files[@]}" "$microvm_manifest_digest" | sha256sum | awk '{print $1}')"
  jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$control_plane_digest" --arg contract "$public_contract_digest" '{image:"control-plane",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64","linux/arm64"],publicContractDigest:$contract}' >"$output_dir/control-plane.oci.json"
  jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$runner_digest" --arg contract "$public_contract_digest" '{image:"runner",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64"],publicContractDigest:$contract}' >"$output_dir/runner.oci.json"
  jq -n --arg version "$version" --arg commit "$source_commit" --arg digest "$microvm_image_digest" --arg manifest "$microvm_manifest_digest" --arg fingerprint "$microvm_fingerprint" '{image:"microvm-artifacts",version:$version,sourceCommit:$commit,digest:$digest,platforms:["linux/amd64"],signedManifestDigest:$manifest,signingKeyFingerprint:$fingerprint}' >"$output_dir/microvm-artifacts.oci.json"
else
  docker buildx build --platform linux/amd64,linux/arm64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "PUBLIC_CONTRACT_DIGEST=$public_contract_digest" --output "type=oci,dest=$output_dir/control-plane.oci.tar" --metadata-file "$output_dir/control-plane.oci.json" "$repo_root"
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "PUBLIC_CONTRACT_DIGEST=$public_contract_digest" --file "$repo_root/runner/Dockerfile" --output "type=oci,dest=$output_dir/runner.oci.tar" --metadata-file "$output_dir/runner.oci.json" "$repo_root"
  docker buildx build --platform linux/amd64 --provenance=false --sbom=false --build-arg "RELEASE_VERSION=$version" --build-arg "SOURCE_COMMIT=$source_commit" --build-arg "SIGNED_MANIFEST_DIGEST=$microvm_manifest_digest" --file "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile" --output "type=oci,dest=$output_dir/microvm-artifacts.oci.tar" --metadata-file "$output_dir/microvm-artifacts.oci.json" "$microvm_source"
  control_plane_digest="$(jq -er '."containerimage.digest"' "$output_dir/control-plane.oci.json")"
  runner_digest="$(jq -er '."containerimage.digest"' "$output_dir/runner.oci.json")"
  microvm_image_digest="$(jq -er '."containerimage.digest"' "$output_dir/microvm-artifacts.oci.json")"
fi

go -C "$repo_root" run ./cmd/secondbox-release-tool standard-documents "$microvm_manifest_digest" "$microvm_runtime_digest" "$microvm_toolchain_digest" "$output_dir"

jq -n --arg version "$version" --arg commit "$source_commit" --arg ts "$typescript_name" --arg go "secondbox-${version}-go-module.tar.gz" '{schemaVersion:1,version:$version,sourceCommit:$commit,typeScriptPackage:$ts,goModuleArchive:$go}' >"$output_dir/secondbox-${version}-package-metadata.json"
jq -n --arg version "$version" --arg commit "$source_commit" '{spdxVersion:"SPDX-2.3",dataLicense:"CC0-1.0",SPDXID:"SPDXRef-DOCUMENT",name:("SecondBox-"+$version),documentNamespace:("https://github.com/SecondStack-AI/SecondBox/releases/tag/v"+$version),creationInfo:{creators:["Organization: SecondStack AI"],comment:("deterministic source commit "+$commit)},packages:[{name:"SecondBox",SPDXID:"SPDXRef-Package-SecondBox",versionInfo:$version,downloadLocation:("git+https://github.com/SecondStack-AI/SecondBox.git@"+$commit),filesAnalyzed:false}]}' >"$output_dir/secondbox-${version}.spdx.json"
jq -n --arg version "$version" --arg commit "$source_commit" --arg contract "$public_contract_digest" --arg guest "$microvm_manifest_digest" '{_type:"https://in-toto.io/Statement/v1",subject:[],predicateType:"https://slsa.dev/provenance/v1",predicate:{buildDefinition:{buildType:"https://github.com/SecondStack-AI/SecondBox/release-stage/v1",externalParameters:{version:$version,sourceCommit:$commit,publicContractDigest:$contract,signedGuestManifestDigest:$guest}},runDetails:{builder:{id:"https://github.com/SecondStack-AI/SecondBox"}}}}' >"$output_dir/secondbox-${version}-provenance.json"
jq -n --arg version "$version" --arg commit "$source_commit" --arg control "$control_plane_digest" --arg runner "$runner_digest" --arg microImage "$microvm_image_digest" --arg microManifest "$microvm_manifest_digest" --arg fingerprint "$microvm_fingerprint" --argjson runtime "$microvm_runtime_bundle" --argjson toolchain "$microvm_toolchain_bundle" '{version:$version,sourceCommit:$commit,controlPlaneDigest:$control,runnerDigest:$runner,microvmImageDigest:$microImage,microvmManifestDigest:$microManifest,microvmSigningKeyFingerprint:$fingerprint,microvmRuntimeBundle:$runtime,microvmToolchainBundle:$toolchain}' >"$temporary/candidate-input.json"
go -C "$repo_root" run ./cmd/secondbox-release-tool manifest "$temporary/candidate-input.json" "$output_dir"

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

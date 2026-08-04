#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d)"
cleanup() { rm -rf "$work_dir"; }
trap cleanup EXIT

artifact_dir="$work_dir/microvm"
mkdir "$artifact_dir"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work_dir/private.pem" >/dev/null 2>&1
openssl pkey -in "$work_dir/private.pem" -pubout -out "$work_dir/public.pem" >/dev/null 2>&1
for name in kernel kernel-provenance.json rootfs-debian-license-inventory.json rootfs-debian-packages.lock rootfs-python-license-inventory.json rootfs-python.freeze rootfs-source-manifest.json rootfs.ext4 runtime-manifest.json secondbox-rootfs-contract.json shared.img toolchain-manifest.json; do
  printf 'synthetic release-stage fixture: %s\n' "$name" >"$artifact_dir/$name"
done
runtime_digest="sha256:$(sha256sum "$artifact_dir/runtime-manifest.json" | awk '{print $1}')"
toolchain_digest="sha256:$(sha256sum "$artifact_dir/toolchain-manifest.json" | awk '{print $1}')"
jq -n --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '{artifactVersion:"synthetic-release-stage",architecture:"amd64",guestProtocol:{minimum:1,maximum:1},runtimeBundle:{artifactId:"synthetic-runtime",path:"runtime-manifest.json",manifestDigest:$runtime,mandatoryGuestFeatures:[]},toolchainBundle:{artifactId:"synthetic-toolchain",path:"toolchain-manifest.json",manifestDigest:$toolchain,mandatoryGuestFeatures:[]}}' >"$artifact_dir/manifest.json"
cp "$work_dir/public.pem" "$artifact_dir/signing.pub"
(
  cd "$artifact_dir"
  sha256sum kernel kernel-provenance.json rootfs-debian-license-inventory.json rootfs-debian-packages.lock rootfs-python-license-inventory.json rootfs-python.freeze rootfs-source-manifest.json rootfs.ext4 runtime-manifest.json secondbox-rootfs-contract.json shared.img toolchain-manifest.json manifest.json >SHA256SUMS
)
openssl dgst -sha256 -sign "$work_dir/private.pem" -out "$artifact_dir/manifest.sig" "$artifact_dir/manifest.json"
fingerprint="$(openssl pkey -pubin -in "$work_dir/public.pem" -outform DER | sha256sum | awk '{print $1}')"

stage_one="$work_dir/stage-one"
stage_two="$work_dir/stage-two"
export SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR="$artifact_dir"
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY="$work_dir/public.pem"
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256="$fingerprint"
"$repo_root/scripts/release-stage.sh" --test-mode 0.1.0 "$stage_one" >/dev/null
"$repo_root/scripts/release-stage.sh" --test-mode 0.1.0 "$stage_two" >/dev/null

for stage in "$stage_one" "$stage_two"; do
  go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage"
  jq -e --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '.schemaVersion == "secondbox.release/artifact-manifest/v2" and .microvm.runtimeBundle.manifestDigest == $runtime and .microvm.toolchainBundle.manifestDigest == $toolchain' "$stage/secondbox-0.1.0-artifact-manifest.json" >/dev/null
  jq -e --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '.schemaVersion == "secondbox.standard-bundle/v2" and .profile.revisions[0].spec.runtimeBundleDigest == $runtime and .profile.revisions[0].spec.toolchainBundleDigest == $toolchain and (.profile.revisions[0].spec.runtimeBundleDigest != .profile.revisions[0].spec.toolchainBundleDigest)' "$stage/durable-coding.standard-bundle.json" >/dev/null
  [[ ! -e "$stage/qualification-attestation.json" && ! -e "$stage/release-index.json" ]] || { echo "local staging manufactured qualification/finalization evidence" >&2; exit 1; }
done

diff -u \
  <(cd "$stage_one" && sha256sum * | sed "s#  #  #" ) \
  <(cd "$stage_two" && sha256sum * | sed "s#  #  #" ) || {
  echo "release staging is not reproducible" >&2
  exit 1
}

touch "$stage_one/unknown-extra"
if go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage_one" >/dev/null 2>&1; then
  echo "release verifier accepted an unknown extra file" >&2
  exit 1
fi
rm "$stage_one/unknown-extra"
printf 'tampered\n' >>"$stage_one/agent-compartment.standard-bundle.json"
if go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage_one" >/dev/null 2>&1; then
  echo "release verifier accepted checksum drift" >&2
  exit 1
fi

if "$repo_root/scripts/release-stage.sh" --test-mode v1.2.3 "$work_dir/bad-version" >/dev/null 2>&1; then
  echo "release staging accepted a noncanonical version" >&2
  exit 1
fi
if "$repo_root/scripts/release-stage.sh" 1.2.3 "$work_dir/dirty-real-stage" >/dev/null 2>&1; then
  echo "real release staging accepted dirty source or an absent exact tag" >&2
  exit 1
fi

if "$repo_root/scripts/release-upload.sh" >/dev/null 2>&1; then
  echo "release upload accepted missing arguments" >&2
  exit 1
fi

#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work_dir="$(mktemp -d)"
qualification_evidence="$repo_root/.tmp/scenario-qualification-evidence.json"
saved_qualification_evidence=false
mkdir -p "$repo_root/.tmp"
if [[ -e "$qualification_evidence" || -L "$qualification_evidence" ]]; then
  cp -a "$qualification_evidence" "$work_dir/original-qualification-evidence"
  saved_qualification_evidence=true
fi
cleanup() {
  rm -f -- "$qualification_evidence"
  if $saved_qualification_evidence; then
    cp -a "$work_dir/original-qualification-evidence" "$qualification_evidence"
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT

rm -f -- "$qualification_evidence"
if "$repo_root/scripts/release-stage.sh" 1.2.3 "$work_dir/missing-evidence" >"$work_dir/missing-evidence.out" 2>&1; then
  echo "release staging accepted absent qualification evidence" >&2
  exit 1
fi
rg -q 'requires qualification evidence.*run just test-scenario' "$work_dir/missing-evidence.out" || {
  echo "release staging did not report actionable missing-evidence guidance" >&2
  exit 1
}
jq -n --arg sourceCommit "0000000000000000000000000000000000000000" '{sourceCommit:$sourceCommit}' >"$qualification_evidence"
if "$repo_root/scripts/release-stage.sh" 1.2.3 "$work_dir/mismatched-evidence" >"$work_dir/mismatched-evidence.out" 2>&1; then
  echo "release staging accepted commit-mismatched qualification evidence" >&2
  exit 1
fi
rg -q 'qualification evidence source commit .* does not match staged source commit' "$work_dir/mismatched-evidence.out" || {
  echo "release staging did not report the qualification commit mismatch" >&2
  exit 1
}
rm -f -- "$qualification_evidence"

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
  jq -e --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '.schemaVersion == "secondbox.release/artifact-manifest/v2" and .microvm.runtimeBundle.manifestDigest == $runtime and .microvm.toolchainBundle.manifestDigest == $toolchain and (.qualificationEvidence.location | endswith("/secondbox-0.1.0-qualification-evidence.json"))' "$stage/secondbox-0.1.0-artifact-manifest.json" >/dev/null
  jq -e --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '.schemaVersion == "secondbox.standard-bundle/v2" and .profile.revisions[0].spec.runtimeBundleDigest == $runtime and .profile.revisions[0].spec.toolchainBundleDigest == $toolchain and (.profile.revisions[0].spec.runtimeBundleDigest != .profile.revisions[0].spec.toolchainBundleDigest)' "$stage/durable-coding.standard-bundle.json" >/dev/null
  jq -e '.schemaVersion == "secondbox.release/qualification-evidence/v1" and .repositoryDirty == false and .suite == "test-scenario" and .passCount == 16 and .wallClockSeconds == 1 and .qualifiedAt == "1970-01-01T00:00:00Z" and .host.kvm.present and .host.tun.present and .host.workspaceFilesystem.type == "xfs"' "$stage/secondbox-0.1.0-qualification-evidence.json" >/dev/null
  rg -q 'secondbox-0.1.0-qualification-evidence.json$' "$stage/SHA256SUMS"
  [[ ! -e "$stage/qualification-attestation.json" && ! -e "$stage/release-index.json" ]] || { echo "local staging resurrected removed attestation/finalization evidence" >&2; exit 1; }
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
printf 'tampered\n' >>"$stage_two/secondbox-0.1.0-qualification-evidence.json"
if go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage_two" >/dev/null 2>&1; then
  echo "release verifier accepted qualification-evidence checksum drift" >&2
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

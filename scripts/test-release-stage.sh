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
stage_dictionary_locale="$work_dir/stage-dictionary-locale"
stage_installer_candidate="$work_dir/stage-installer-candidate"
export SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR="$artifact_dir"
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY="$work_dir/public.pem"
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256="$fingerprint"
# Both reproducibility stagings run under byte collation and the third under
# dictionary collation, so the comparison below means the same thing whatever
# locale the operator brought. The dictionary locale is a stated prerequisite
# rather than an opportunistic check: without it the third staging would silently
# repeat the byte-ordered ones and assert nothing.
dictionary_locale="en_US.UTF-8"
collation_sample=$'SHA256SUMS\nagent-compartment.standard-bundle.json\nsecondbox-deploy_0.7.0_linux_amd64\nsecondbox_0.7.0_linux_amd64'
if [[ "$(LC_ALL="$dictionary_locale" sort <<<"$collation_sample" 2>/dev/null)" == "$(LC_ALL=C sort <<<"$collation_sample")" ]]; then
  echo "release staging locale test requires a dictionary-collating $dictionary_locale locale; generate it and rerun" >&2
  exit 1
fi
LC_ALL=C "$repo_root/scripts/release-stage.sh" --test-mode 0.7.0 "$stage_one" >/dev/null
LC_ALL=C "$repo_root/scripts/release-stage.sh" --test-mode 0.7.0 "$stage_two" >/dev/null
LC_ALL="$dictionary_locale" "$repo_root/scripts/release-stage.sh" --test-mode 0.7.0 "$stage_dictionary_locale" >/dev/null
LC_ALL=C "$repo_root/scripts/release-stage.sh" --test-mode --candidate 0.7.0 "$stage_installer_candidate" >/dev/null

jq -e '.candidate == true and .installerQualificationEvidence == {location:"",digest:""}' "$stage_installer_candidate/secondbox-0.7.0-artifact-manifest.json" >/dev/null
[[ ! -e "$stage_installer_candidate/secondbox-0.7.0-installer-qualification-evidence.json" ]] || { echo "installer candidate claimed final qualification evidence" >&2; exit 1; }
candidate_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$stage_installer_candidate/secondbox-0.7.0-artifact-manifest.json")"
final_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$stage_one/secondbox-0.7.0-artifact-manifest.json")"
[[ "$candidate_subject" == "$final_subject" ]] || { echo "installer candidate and final release subjects differ" >&2; exit 1; }

for stage in "$stage_one" "$stage_two" "$stage_dictionary_locale"; do
  go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage"
  source_commit="$(jq -er '.sourceCommit' "$stage/secondbox-0.7.0-artifact-manifest.json")"
  jq -e --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '.schemaVersion == "secondbox.release/artifact-manifest/v5" and .version == "0.7.0" and .runnerProtocol == {minimum:3,maximum:3} and (.installerTools.reference | startswith("ghcr.io/secondstack-ai/secondbox/installer-tools@sha256:")) and (.installBootstrap.location | endswith("/v0.7.0/install.sh")) and .microvm.runtimeBundle.manifestDigest == $runtime and .microvm.toolchainBundle.manifestDigest == $toolchain and (.qualificationEvidence.location | endswith("/secondbox-0.7.0-qualification-evidence.json")) and (.installerQualificationEvidence.location | endswith("/secondbox-0.7.0-installer-qualification-evidence.json"))' "$stage/secondbox-0.7.0-artifact-manifest.json" >/dev/null
  jq -e --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '.schemaVersion == "secondbox.standard-bundle/v2" and .profile.revisions[-1].spec.runtimeBundleDigest == $runtime and .profile.revisions[-1].spec.toolchainBundleDigest == $toolchain and (.profile.revisions[-1].spec.runtimeBundleDigest != .profile.revisions[-1].spec.toolchainBundleDigest)' "$stage/durable-coding.standard-bundle.json" >/dev/null
  jq -e '.name == "agent-compartment-isolated" and .logicalGateway == "" and .profile.name == "agent-compartment-isolated" and .profile.revisions[-1].spec.network == {mode:"deny_all",destinations:[]} and .profile.revisions[-1].spec.ports == []' "$stage/agent-compartment-isolated.standard-bundle.json" >/dev/null
  jq -e --arg commit "$source_commit" '.schemaVersion == "secondbox.release/qualification-evidence/v2" and .sourceCommit == $commit and .repositoryDirty == false and .suite == "test-scenario" and .passCount == 16 and .wallClockSeconds == 1 and .qualifiedAt == "1970-01-01T00:00:00Z" and .host.platform == "linux-amd64" and .host.kvm.present and .host.tun.present and .host.workspaceFilesystem.type == "xfs"' "$stage/secondbox-0.7.0-qualification-evidence.json" >/dev/null
	jq -e --arg commit "$source_commit" '.schemaVersion == "secondbox.release/installer-qualification-evidence/v2" and .sourceCommit == $commit and .repositoryDirty == false and .suite == "test-installer-qualified" and .passCount == 24 and .wallClockSeconds == 1 and .rebootPassed == true and .host.platform == "linux-amd64" and .host.kvm.present and .host.tun.present and .host.workspaceFilesystem.type == "xfs"' "$stage/secondbox-0.7.0-installer-qualification-evidence.json" >/dev/null
  jq -e --arg commit "$source_commit" '.version == "0.7.0" and .sourceCommit == $commit and .typeScriptPackage == "secondstack-ai-secondbox-0.7.0.tgz" and .goModuleArchive == "secondbox-0.7.0-go-module.tar.gz"' "$stage/secondbox-0.7.0-package-metadata.json" >/dev/null
  tar -xOf "$stage/secondbox-0.7.0-go-module.tar.gz" go.mod | rg -q '^retract v0\.7\.0 // The public Go proxy cached an intermediate release-preparation commit; use v0\.7\.1\.$'
  jq -e --arg commit "$source_commit" '.name == "SecondBox-0.7.0" and .packages == [{name:"SecondBox",SPDXID:"SPDXRef-Package-SecondBox",versionInfo:"0.7.0",downloadLocation:("git+https://github.com/SecondStack-AI/SecondBox.git@"+$commit),filesAnalyzed:false}] and (.creationInfo.comment | endswith($commit))' "$stage/secondbox-0.7.0.spdx.json" >/dev/null
  for image in control-plane runner installer-tools microvm-artifacts; do
    jq -e --arg commit "$source_commit" '.version == "0.7.0" and .sourceCommit == $commit' "$stage/$image.oci.json" >/dev/null
  done
  tar -xOf "$stage/secondstack-ai-secondbox-0.7.0.tgz" package/package.json | jq -e '.version == "0.7.0"' >/dev/null
  [[ "$(sha256sum "$stage/secondbox-0.7.0-openapi.json" | awk '{print "sha256:"$1}')" == "$(jq -er '.openapi.digest' "$stage/secondbox-0.7.0-artifact-manifest.json")" ]]
  rg -q 'secondbox-0.7.0-qualification-evidence.json$' "$stage/SHA256SUMS"
  rg -q 'secondbox-0.7.0-installer-qualification-evidence.json$' "$stage/SHA256SUMS"
  rg -q 'install.sh$' "$stage/SHA256SUMS"
  rg -q "version='0.7.0'" "$stage/install.sh"
  rg -q "expected_sha256='$(sha256sum "$stage/secondbox-deploy_0.7.0_linux_amd64" | awk '{print $1}')'" "$stage/install.sh"
  rg -qF '"$binary" "$command" "$@" &' "$stage/install.sh"
  rg -qF "trap 'terminate TERM 143' TERM" "$stage/install.sh"
  if rg -qF '/proc/self/fd' "$stage/install.sh" || rg -qF 'exec "$binary"' "$stage/install.sh"; then
    echo "install bootstrap removes or hides the executable needed for sudo re-exec" >&2
    exit 1
  fi
  [[ "$(stat -c '%a' "$stage/install.sh")" == 644 ]] || { echo "install bootstrap mode is not 0644" >&2; exit 1; }
  [[ ! -e "$stage/qualification-attestation.json" && ! -e "$stage/release-index.json" ]] || { echo "local staging resurrected removed attestation/finalization evidence" >&2; exit 1; }
done

diff -u \
  <(cd "$stage_one" && sha256sum * | sed "s#  #  #" ) \
  <(cd "$stage_two" && sha256sum * | sed "s#  #  #" ) || {
  echo "release staging is not reproducible" >&2
  exit 1
}
diff -u \
  <(cd "$stage_one" && sha256sum * | sed "s#  #  #" ) \
  <(cd "$stage_dictionary_locale" && sha256sum * | sed "s#  #  #" ) || {
  echo "release staging is not reproducible across operator locales" >&2
  exit 1
}

touch "$stage_one/unknown-extra"
if go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage_one" >/dev/null 2>&1; then
  echo "release verifier accepted an unknown extra file" >&2
  exit 1
fi
rm "$stage_one/unknown-extra"
cp "$stage_one/candidate-allowlist.json" "$work_dir/ordered-allowlist.json"
jq '{schemaVersion:.schemaVersion,files:(.files|reverse)}' "$work_dir/ordered-allowlist.json" >"$stage_one/candidate-allowlist.json"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage_one" >/dev/null || {
  echo "release verifier rejected a complete candidate whose allowlist order differs from the directory order" >&2
  exit 1
}
cp "$work_dir/ordered-allowlist.json" "$stage_one/candidate-allowlist.json"
printf 'tampered\n' >>"$stage_one/agent-compartment.standard-bundle.json"
if go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage_one" >/dev/null 2>&1; then
  echo "release verifier accepted checksum drift" >&2
  exit 1
fi
printf 'tampered\n' >>"$stage_two/secondbox-0.7.0-qualification-evidence.json"
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

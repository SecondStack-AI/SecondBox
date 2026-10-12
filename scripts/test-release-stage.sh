#!/usr/bin/env bash
set -euo pipefail
export RELEASE_IMAGE_PLATFORMS=linux/amd64,linux/arm64

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

shared="$work_dir/shared"
rm -f -- "$qualification_evidence"
if "$repo_root/scripts/release-stage.sh" --shared-from "$shared" 1.2.3 "$work_dir/missing-evidence" >"$work_dir/missing-evidence.out" 2>&1; then
  echo "release staging accepted absent qualification evidence" >&2
  exit 1
fi
rg -q 'requires qualification evidence.*run just test-scenario' "$work_dir/missing-evidence.out" || {
  echo "release staging did not report actionable missing-evidence guidance" >&2
  exit 1
}
jq -n --arg sourceCommit "0000000000000000000000000000000000000000" '{sourceCommit:$sourceCommit}' >"$qualification_evidence"
if "$repo_root/scripts/release-stage.sh" --shared-from "$shared" 1.2.3 "$work_dir/mismatched-evidence" >"$work_dir/mismatched-evidence.out" 2>&1; then
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
for name in kernel kernel-provenance.json rootfs-debian-license-inventory.json rootfs-debian-packages.lock rootfs-python-license-inventory.json rootfs-python.freeze rootfs-source-manifest.json rootfs.ext4 runtime-manifest.json secondbox-rootfs-contract.json shared.img toolchain-manifest.json; do
  printf 'synthetic release-stage fixture: %s\n' "$name" >"$artifact_dir/$name"
done
runtime_digest="sha256:$(sha256sum "$artifact_dir/runtime-manifest.json" | awk '{print $1}')"
toolchain_digest="sha256:$(sha256sum "$artifact_dir/toolchain-manifest.json" | awk '{print $1}')"
jq -n --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" '{artifactVersion:"synthetic-release-stage",architecture:"amd64",guestProtocol:{minimum:1,maximum:1},runtimeBundle:{artifactId:"synthetic-runtime",path:"runtime-manifest.json",manifestDigest:$runtime,mandatoryGuestFeatures:[]},toolchainBundle:{artifactId:"synthetic-toolchain",path:"toolchain-manifest.json",manifestDigest:$toolchain,mandatoryGuestFeatures:[]}}' >"$artifact_dir/manifest.json"
(
  cd "$artifact_dir"
  sha256sum kernel kernel-provenance.json rootfs-debian-license-inventory.json rootfs-debian-packages.lock rootfs-python-license-inventory.json rootfs-python.freeze rootfs-source-manifest.json rootfs.ext4 runtime-manifest.json secondbox-rootfs-contract.json shared.img toolchain-manifest.json manifest.json >SHA256SUMS
)
bundle_manifest_digest="sha256:$(sha256sum "$artifact_dir/manifest.json" | awk '{print $1}')"

stage_one="$work_dir/stage-one"
stage_two="$work_dir/stage-two"
stage_dictionary_locale="$work_dir/stage-dictionary-locale"
stage_installer_candidate="$work_dir/stage-installer-candidate"
export SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR="$artifact_dir"
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

# The architecture-neutral files are built once and are reproducible across
# builds and operator locales; every staging below consumes the same set.
LC_ALL=C "$repo_root/scripts/release-build-shared.sh" --test-mode 0.7.0 "$shared" >/dev/null
LC_ALL="$dictionary_locale" "$repo_root/scripts/release-build-shared.sh" --test-mode 0.7.0 "$work_dir/shared-again" >/dev/null
diff -r "$shared" "$work_dir/shared-again" || { echo "shared release build is not reproducible" >&2; exit 1; }
shared_commit="$(jq -er '.sourceCommit' "$shared/shared-artifacts.json")"
jq -e --arg commit "$shared_commit" '.schemaVersion == "secondbox.release/shared-artifacts/v1" and .version == "0.7.0" and .tag == "v0.7.0" and .sourceCommit == $commit and (.postgresImage | test("^docker.io/library/postgres@sha256:[0-9a-f]{64}$")) and ([.files[].name] == ([.files[].name] | sort)) and (.files | length == 14)' "$shared/shared-artifacts.json" >/dev/null
while IFS=$'\t' read -r name digest; do
  [[ "sha256:$(sha256sum "$shared/$name" | awk '{print $1}')" == "$digest" ]] || { echo "shared artifacts record misstates $name" >&2; exit 1; }
done < <(jq -r '.files[] | [.name, .digest] | @tsv' "$shared/shared-artifacts.json")
[[ "$(stat -c '%a' "$shared/secondbox-deploy_0.7.0_linux_amd64")" == 755 && "$(stat -c '%a' "$shared/install.sh")" == 644 ]] || { echo "shared release build modes differ from the contract" >&2; exit 1; }
if "$repo_root/scripts/release-build-shared.sh" --test-mode v0.7.0 "$work_dir/shared-bad-version" >/dev/null 2>&1; then
  echo "shared release build accepted a noncanonical version" >&2; exit 1
fi
if "$repo_root/scripts/release-build-shared.sh" 1.2.3 "$work_dir/shared-real" >/dev/null 2>&1; then
  echo "real shared release build accepted dirty source or an absent exact tag" >&2; exit 1
fi

LC_ALL=C "$repo_root/scripts/release-stage.sh" --test-mode --shared-from "$shared" 0.7.0 "$stage_one" >/dev/null
LC_ALL=C "$repo_root/scripts/release-stage.sh" --test-mode --shared-from "$shared" 0.7.0 "$stage_two" >/dev/null
LC_ALL="$dictionary_locale" "$repo_root/scripts/release-stage.sh" --test-mode --shared-from "$shared" 0.7.0 "$stage_dictionary_locale" >/dev/null
LC_ALL=C "$repo_root/scripts/release-stage.sh" --test-mode --candidate --shared-from "$shared" 0.7.0 "$stage_installer_candidate" >/dev/null
# Staging publishes every shared file byte for byte with its mode, and nothing of the record.
while IFS= read -r name; do
  cmp -s "$shared/$name" "$stage_one/$name" && [[ "$(stat -c '%a' "$shared/$name")" == "$(stat -c '%a' "$stage_one/$name")" ]] || { echo "release staging changed shared file $name" >&2; exit 1; }
done < <(jq -r '.files[].name' "$shared/shared-artifacts.json")
[[ ! -e "$stage_one/shared-artifacts.json" ]] || { echo "release staging published the shared artifacts record" >&2; exit 1; }

# A shared set that is incomplete, extended, tampered, re-moded, or recorded
# for another version or commit is rejected before anything is built.
reject_shared() {
  local label="$1" directory="$2" version="${3:-0.7.0}"
  if "$repo_root/scripts/release-stage.sh" --test-mode --shared-from "$directory" "$version" "$work_dir/rejected-stage" >"$work_dir/rejected-stage.out" 2>&1; then
    echo "release staging accepted a shared set with $label" >&2; exit 1
  fi
  rm -rf "$work_dir/rejected-stage" "$directory"
}
tampered_shared() { rm -rf "$work_dir/shared-tampered"; cp -a "$shared" "$work_dir/shared-tampered"; }
tampered_shared; printf 'tampered' >>"$work_dir/shared-tampered/secondbox_0.7.0_darwin_arm64"
reject_shared 'a tampered binary' "$work_dir/shared-tampered"
tampered_shared; rm "$work_dir/shared-tampered/secondbox-0.7.0.spdx.json"
reject_shared 'a missing file' "$work_dir/shared-tampered"
tampered_shared; printf extra >"$work_dir/shared-tampered/extra"
reject_shared 'an undeclared file' "$work_dir/shared-tampered"
tampered_shared; chmod 0644 "$work_dir/shared-tampered/secondbox-deploy_0.7.0_linux_arm64"
reject_shared 'a non-executable binary' "$work_dir/shared-tampered"
tampered_shared; rm "$work_dir/shared-tampered/shared-artifacts.json"
reject_shared 'no record' "$work_dir/shared-tampered"
tampered_shared; jq '.sourceCommit = ("0" * 40)' "$shared/shared-artifacts.json" >"$work_dir/shared-tampered/shared-artifacts.json"
reject_shared 'another source commit' "$work_dir/shared-tampered"
tampered_shared
reject_shared 'another version' "$work_dir/shared-tampered" 0.7.1
# A deploy binary swapped together with its record digest no longer matches install.sh.
tampered_shared; printf 'tampered' >>"$work_dir/shared-tampered/secondbox-deploy_0.7.0_linux_amd64"
jq --arg digest "sha256:$(sha256sum "$work_dir/shared-tampered/secondbox-deploy_0.7.0_linux_amd64" | awk '{print $1}')" '(.files[] | select(.name == "secondbox-deploy_0.7.0_linux_amd64") | .digest) = $digest' "$shared/shared-artifacts.json" >"$work_dir/shared-tampered/shared-artifacts.json"
reject_shared 'an install bootstrap for another deploy binary' "$work_dir/shared-tampered"
rg -q 'shared install bootstrap lacks expected_sha256=' "$work_dir/rejected-stage.out" || { echo "release staging did not explain the install bootstrap mismatch" >&2; exit 1; }

# An unbound build must bind to exactly the same candidate/final bytes, without
# rebuilding; corruption, undeclared entries and another version are rejected.
stage_build="$work_dir/stage-build"
"$repo_root/scripts/release-stage.sh" --test-mode --build-only --shared-from "$shared" 0.7.0 "$stage_build" >/dev/null
[[ ! -e "$stage_build/secondbox-0.7.0-artifact-manifest.json" ]]
"$repo_root/scripts/release-stage.sh" --test-mode --candidate --from-build "$stage_build" 0.7.0 "$work_dir/bound-candidate" >/dev/null
"$repo_root/scripts/release-stage.sh" --test-mode --from-build "$stage_build" 0.7.0 "$work_dir/bound-final" >/dev/null
diff -r "$stage_installer_candidate" "$work_dir/bound-candidate"
diff -r "$stage_one" "$work_dir/bound-final"
for rejected in "--from-build $stage_build --shared-from $shared" "--build-only" "--build-only --candidate --shared-from $shared"; do
  # shellcheck disable=SC2086
  if "$repo_root/scripts/release-stage.sh" --test-mode $rejected 0.7.0 "$work_dir/rejected-options" >/dev/null 2>&1; then
    echo "amd64 staging accepted options: $rejected" >&2; exit 1
  fi
  rm -rf "$work_dir/rejected-options"
done
if "$repo_root/scripts/release-stage.sh" --test-mode --candidate --from-build "$stage_build" 0.7.1 "$work_dir/wrong-build-version" >/dev/null 2>&1; then
  echo 'release binding accepted another version' >&2; exit 1
fi
printf undeclared >"$stage_build/extra"
if "$repo_root/scripts/release-stage.sh" --test-mode --candidate --from-build "$stage_build" 0.7.0 "$work_dir/extra-build-file" >/dev/null 2>&1; then
  echo 'release binding accepted an undeclared file' >&2; exit 1
fi
rm -- "$stage_build/extra"
printf corrupt >>"$stage_build/secondbox-0.7.0-openapi.json"
if "$repo_root/scripts/release-stage.sh" --test-mode --candidate --from-build "$stage_build" 0.7.0 "$work_dir/corrupt-build" >/dev/null 2>&1; then
  echo 'release binding accepted corrupt bytes' >&2; exit 1
fi

jq -e '.candidate == true and .installerQualificationEvidence == {location:"",digest:""}' "$stage_installer_candidate/secondbox-0.7.0-artifact-manifest.json" >/dev/null
[[ ! -e "$stage_installer_candidate/secondbox-0.7.0-installer-qualification-evidence.json" ]] || { echo "installer candidate claimed final qualification evidence" >&2; exit 1; }
candidate_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$stage_installer_candidate/secondbox-0.7.0-artifact-manifest.json")"
final_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "$stage_one/secondbox-0.7.0-artifact-manifest.json")"
[[ "$candidate_subject" == "$final_subject" ]] || { echo "installer candidate and final release subjects differ" >&2; exit 1; }

for stage in "$stage_one" "$stage_two" "$stage_dictionary_locale"; do
  go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$stage"
  source_commit="$(jq -er '.sourceCommit' "$stage/secondbox-0.7.0-artifact-manifest.json")"
  jq -e --arg runtime "$runtime_digest" --arg toolchain "$toolchain_digest" --arg bundleManifest "$bundle_manifest_digest" '.schemaVersion == "secondbox.release/artifact-manifest/v7" and .microvm.bundleManifestDigest == $bundleManifest and (.microvm | has("signedManifestDigest") or has("signingKeyFingerprint") | not) and .version == "0.7.0" and .runnerProtocol == {minimum:7,maximum:7} and (.installerTools.reference | startswith("ghcr.io/secondstack-ai/secondbox/installer-tools@sha256:")) and (.installBootstrap.location | endswith("/v0.7.0/install.sh")) and .microvm.runtimeBundle.manifestDigest == $runtime and .microvm.toolchainBundle.manifestDigest == $toolchain and (.qualificationEvidence.location | endswith("/secondbox-0.7.0-qualification-evidence.json")) and (.installerQualificationEvidence.location | endswith("/secondbox-0.7.0-installer-qualification-evidence.json"))' "$stage/secondbox-0.7.0-artifact-manifest.json" >/dev/null
  jq -e '.schemaVersion == "secondbox.standard-bundle/v4" and .profile.revisions[-1].number == 2 and ([.profile.revisions[].spec | has("runtimeBundleDigest") or has("toolchainBundleDigest")] | any | not) and .profile.revisions[-1].spec.network.requiresTenantEgressContext == true' "$stage/durable-coding.standard-bundle.json" >/dev/null
  # Revision 3 repeats revision 2 once Profiles stopped naming bundles; attributed
  # execution, unlimited lifetimes, and connection limits append later revisions.
  jq -e '.name == "agent-compartment" and ([.profile.revisions[].number] == [1,2,3,4,5,6]) and .profile.revisions[2].spec.attributedExecution == null and .profile.revisions[3].spec.attributedExecution == {gateway:"agent-gateway.secondbox.internal",maximumConnections:2} and .profile.revisions[-1].spec.lifecycle.maximumDurationSeconds == null and .profile.revisions[-1].spec.lifecycleCeiling == {idleSeconds:null,maximumDurationSeconds:null} and .profile.revisions[-1].spec.network.requiresTenantEgressContext == true' "$stage/agent-compartment.standard-bundle.json" >/dev/null
  jq -e '.name == "agent-compartment-isolated" and .logicalGateway == "" and .profile.name == "agent-compartment-isolated" and .profile.revisions[-1].number == 3 and .profile.revisions[-1].spec.lifecycle.maximumDurationSeconds == null and .profile.revisions[-1].spec.lifecycleCeiling == {idleSeconds:null,maximumDurationSeconds:null} and .profile.revisions[-1].spec.network == {mode:"deny_all",destinations:[],requiresTenantEgressContext:false} and .profile.revisions[-1].spec.ports == []' "$stage/agent-compartment-isolated.standard-bundle.json" >/dev/null
  jq -e --arg commit "$source_commit" '.schemaVersion == "secondbox.release/qualification-evidence/v2" and .sourceCommit == $commit and .repositoryDirty == false and .suite == "test-scenario" and .passCount == 16 and .wallClockSeconds == 1 and .qualifiedAt == "1970-01-01T00:00:00Z" and .host.platform == "linux-amd64" and .host.kvm.present and .host.tun.present and .host.workspaceFilesystem.type == "xfs"' "$stage/secondbox-0.7.0-qualification-evidence.json" >/dev/null
	jq -e --arg commit "$source_commit" '.schemaVersion == "secondbox.release/installer-qualification-evidence/v2" and .sourceCommit == $commit and .repositoryDirty == false and .suite == "test-installer-qualified" and .passCount == 24 and .wallClockSeconds == 1 and .rebootPassed == true and .host.platform == "linux-amd64" and .host.kvm.present and .host.tun.present and .host.workspaceFilesystem.type == "xfs"' "$stage/secondbox-0.7.0-installer-qualification-evidence.json" >/dev/null
  jq -e --arg commit "$source_commit" '.version == "0.7.0" and .sourceCommit == $commit and .typeScriptPackage == "secondstack-ai-secondbox-0.7.0.tgz" and .goModuleArchive == "secondbox-0.7.0-go-module.tar.gz"' "$stage/secondbox-0.7.0-package-metadata.json" >/dev/null
  tar -xOf "$stage/secondbox-0.7.0-go-module.tar.gz" go.mod | rg -q '^retract v0\.7\.0 // The public Go proxy cached an intermediate release-preparation commit; use v0\.7\.1\.$'
  tar -xOf "$stage/secondbox-0.7.0-go-module.tar.gz" go.mod | rg -q '^retract \[v0\.8\.0, v0\.8\.2\] // Pre-release tags cut before qualification completed \(v0\.8\.0 has an unresolvable module graph; v0\.8\.1 and v0\.8\.2 predate the qualification-driver fixes\); use v0\.8\.3\.$'
  tar -xOf "$stage/secondbox-0.7.0-go-module.tar.gz" go.mod | rg -q '^retract v0\.9\.0 // Tagged before the installer qualification driver accepted the v6 artifact manifest; use v0\.9\.1\.$'
  jq -e --arg commit "$source_commit" '.name == "SecondBox-0.7.0" and .packages == [{name:"SecondBox",SPDXID:"SPDXRef-Package-SecondBox",versionInfo:"0.7.0",downloadLocation:("git+https://github.com/SecondStack-AI/SecondBox.git@"+$commit),filesAnalyzed:false}] and (.creationInfo.comment | endswith($commit))' "$stage/secondbox-0.7.0.spdx.json" >/dev/null
  for image in control-plane runner installer-tools microvm-artifacts runner-gvisor gvisor-artifacts; do
    jq -e --arg commit "$source_commit" '.version == "0.7.0" and .sourceCommit == $commit' "$stage/$image.oci.json" >/dev/null
  done
  tar -xOf "$stage/secondstack-ai-secondbox-0.7.0.tgz" package/package.json | jq -e '.version == "0.7.0"' >/dev/null
  [[ "$(sha256sum "$stage/secondbox-0.7.0-openapi.json" | awk '{print "sha256:"$1}')" == "$(jq -er '.openapi.digest' "$stage/secondbox-0.7.0-artifact-manifest.json")" ]]
  rg -q 'secondbox-0.7.0-qualification-evidence.json$' "$stage/SHA256SUMS"
  rg -q 'secondbox-0.7.0-installer-qualification-evidence.json$' "$stage/SHA256SUMS"
  rg -q 'secondbox-0.7.0-gvisor-qualification-evidence.json$' "$stage/SHA256SUMS"
  rg -q 'secondbox-0.7.0-gvisor-pod-qualification-evidence.json$' "$stage/SHA256SUMS"
  rg -q 'secondbox-0.7.0-gvisor-materialization.json$' "$stage/SHA256SUMS"
  jq -e '.gvisor.runscRelease != "" and (.gvisor.materializationDigest | test("^sha256:[0-9a-f]{64}$")) and (.gvisor.runnerReference | startswith("ghcr.io/secondstack-ai/secondbox/runner-gvisor@sha256:")) and (.gvisor.imageReference | startswith("ghcr.io/secondstack-ai/secondbox/gvisor-artifacts@sha256:"))' "$stage/secondbox-0.7.0-artifact-manifest.json" >/dev/null
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

if "$repo_root/scripts/release-stage.sh" --test-mode --shared-from "$shared" v1.2.3 "$work_dir/bad-version" >/dev/null 2>&1; then
  echo "release staging accepted a noncanonical version" >&2
  exit 1
fi
if "$repo_root/scripts/release-stage.sh" --shared-from "$shared" 1.2.3 "$work_dir/dirty-real-stage" >/dev/null 2>&1; then
  echo "real release staging accepted dirty source or an absent exact tag" >&2
  exit 1
fi

if "$repo_root/scripts/release-upload.sh" >/dev/null 2>&1; then
  echo "release upload accepted missing arguments" >&2
  exit 1
fi

RELEASE_IMAGE_PLATFORMS=linux/amd64 "$repo_root/scripts/release-stage.sh" --test-mode --candidate --shared-from "$shared" 0.7.0 "$work_dir/lean"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$work_dir/lean"
jq -e '.platforms.controlPlane == ["linux/amd64"] and (.platforms.hostBinaries | length == 4) and .gvisor.podQualificationEvidence == {location:"",digest:""}' "$work_dir/lean/secondbox-0.7.0-artifact-manifest.json" >/dev/null
[[ ! -e "$work_dir/lean/secondbox-0.7.0-gvisor-pod-qualification-evidence.json" ]]

# The arm64 artifact set carries an arm64 bundle, arm64 standard Profiles and no
# gVisor, and stages the same shared files as the amd64 set, independently of it.
amd64_final="$work_dir/bound-final"
arm64_stage() { RELEASE_GUEST_ARCHITECTURE=arm64 RELEASE_IMAGE_PLATFORMS=linux/arm64 "$repo_root/scripts/release-stage.sh" --test-mode "$@"; }
if arm64_stage --shared-from "$shared" 0.7.0 "$work_dir/arm64-with-amd64-bundle" >/dev/null 2>&1; then
  echo "release staging accepted an amd64 microVM bundle for an arm64 release" >&2
  exit 1
fi
arm64_artifact_dir="$work_dir/microvm-arm64"
cp -a "$artifact_dir" "$arm64_artifact_dir"
jq '.architecture = "arm64"' "$artifact_dir/manifest.json" >"$arm64_artifact_dir/manifest.json"
(
  cd "$arm64_artifact_dir"
  sha256sum kernel kernel-provenance.json rootfs-debian-license-inventory.json rootfs-debian-packages.lock rootfs-python-license-inventory.json rootfs-python.freeze rootfs-source-manifest.json rootfs.ext4 runtime-manifest.json secondbox-rootfs-contract.json shared.img toolchain-manifest.json manifest.json >SHA256SUMS
)
export SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR="$arm64_artifact_dir"
for rejected in "--candidate --shared-from $shared" "--build-only --shared-from $shared" "--shared-from $amd64_final" ""; do
  # shellcheck disable=SC2086
  if arm64_stage $rejected 0.7.0 "$work_dir/arm64-rejected" >/dev/null 2>&1; then
    echo "arm64 staging accepted options: ${rejected:-none}" >&2
    exit 1
  fi
  rm -rf "$work_dir/arm64-rejected"
done
arm64_stage --shared-from "$shared" 0.7.0 "$work_dir/arm64" >/dev/null
arm64_stage --shared-from "$shared" 0.7.0 "$work_dir/arm64-again" >/dev/null
diff -r "$work_dir/arm64" "$work_dir/arm64-again"
go -C "$repo_root" run ./cmd/secondbox-release-tool verify "$work_dir/arm64"
arm64_manifest="$work_dir/arm64/secondbox-0.7.0-arm64-artifact-manifest.json"
[[ ! -e "$work_dir/arm64/secondbox-0.7.0-artifact-manifest.json" && -f "$work_dir/arm64/SHA256SUMS-arm64" && -f "$work_dir/arm64/candidate-allowlist-arm64.json" ]]
jq -e --slurpfile amd64 "$amd64_final/secondbox-0.7.0-artifact-manifest.json" '
  (.candidate // false) == false and .gvisor == null and .installerQualificationEvidence == {location:"",digest:""} and
  .platforms.controlPlane == ["linux/arm64"] and .platforms.runner == ["linux/arm64"] and .platforms.guest == ["linux/arm64"] and .platforms.qualifiedRunnerGuest == ["linux/arm64"] and
  (.qualificationEvidence.location | endswith("/v0.7.0/secondbox-0.7.0-arm64-qualification-evidence.json")) and
  ([.standardBundles[].document.location | endswith("-arm64.standard-bundle.json")] | all) and
  .openapi == $amd64[0].openapi and .goSdk == $amd64[0].goSdk and .typeScriptSdk == $amd64[0].typeScriptSdk and .binaries == $amd64[0].binaries and
  .installBootstrap == $amd64[0].installBootstrap and .sboms == $amd64[0].sboms and .bundledServices == $amd64[0].bundledServices' "$arm64_manifest" >/dev/null
jq -e '.architecture == "arm64" and .runnerPoolSelector == "standard-arm64"' "$work_dir/arm64/agent-compartment-arm64.standard-bundle.json" >/dev/null
jq -e '.host.platform == "linux-arm64"' "$work_dir/arm64/secondbox-0.7.0-arm64-qualification-evidence.json" >/dev/null
for image in control-plane runner installer-tools microvm-artifacts; do [[ -f "$work_dir/arm64/$image-arm64.oci.json" ]]; done
[[ -z "$(find "$work_dir/arm64" -name '*gvisor*' -o -name '*installer-qualification-evidence*' -o -name 'agent-compartment.standard-bundle.json')" ]]
# Both artifact sets publish under one tag: every shared name carries identical bytes.
while IFS= read -r name; do
  [[ ! -e "$amd64_final/$name" ]] || cmp -s "$amd64_final/$name" "$work_dir/arm64/$name" || { echo "arm64 artifact set changed shared file $name" >&2; exit 1; }
done < <(ls "$work_dir/arm64")
tampered_shared; printf 'tampered' >>"$work_dir/shared-tampered/secondbox_0.7.0_linux_arm64"
if arm64_stage --shared-from "$work_dir/shared-tampered" 0.7.0 "$work_dir/arm64-tampered" >/dev/null 2>&1; then
  echo "arm64 staging accepted a shared binary that differs from the shared artifacts record" >&2
  exit 1
fi

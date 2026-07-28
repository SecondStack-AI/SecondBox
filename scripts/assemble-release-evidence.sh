#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 3 ]]; then
  echo "Usage: scripts/assemble-release-evidence.sh EVIDENCE_DIRECTORY RELEASE_VERSION SOURCE_COMMIT" >&2
  exit 2
fi

evidence_directory="$(realpath -e -- "$1")"
release_version="$2"
source_commit="$3"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
generated_at="${SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP:?set SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP}"
output_path="$evidence_directory/release-evidence.json"
subject_manifest_path="$evidence_directory/release-subjects.json"

if [[ -L "$1" || ! -d "$evidence_directory" ]]; then
  echo "SecondBox release evidence output must be an existing non-symbolic-link directory" >&2
  exit 1
fi
if [[ -e "$output_path" ]]; then
  echo "Refusing to overwrite release evidence: $output_path" >&2
  exit 1
fi
if [[ ! "$source_commit" =~ ^[0-9a-f]{40}$ ]]; then
  echo "SecondBox release evidence requires a 40-character lowercase source commit" >&2
  exit 1
fi
if [[ -L "$subject_manifest_path" || ! -f "$subject_manifest_path" ]]; then
  echo "SecondBox release evidence requires release-subjects.json at the evidence root" >&2
  exit 1
fi
if ! jq -e \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" '
    .releaseVersion == $releaseVersion and
    .sourceCommit == $sourceCommit
  ' "$subject_manifest_path" >/dev/null; then
  echo "SecondBox release evidence subject manifest candidate identity mismatch" >&2
  exit 1
fi

install -m 0644 \
  "$repo_root/release/current-compatibility.json" \
  "$evidence_directory/current-compatibility.json"

working_directory="$(mktemp -d)"
cleanup_release_evidence_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox release evidence failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_evidence_working_directory EXIT

artifact_record() {
  local file_path="$1"
  local canonical_path
  local relative_path

  if [[ -L "$file_path" || ! -f "$file_path" ]]; then
    return 1
  fi
  canonical_path="$(realpath -e -- "$file_path")"
  if [[ "$canonical_path" != "$evidence_directory"/* ]]; then
    echo "SecondBox release evidence artifact is outside the evidence directory: $file_path" >&2
    return 1
  fi
  relative_path="${canonical_path#"$evidence_directory"/}"
  jq -cn \
    --arg path "$relative_path" \
    --arg sha256 "$(sha256sum "$canonical_path" | awk '{print $1}')" \
    '{path: $path, sha256: $sha256}'
}

artifacts_in_directory() {
  local directory_path="$1"
  local records_path="$working_directory/artifacts-$(printf '%s' "$directory_path" | sha256sum | awk '{print $1}').jsonl"
  : >"$records_path"
  if [[ -d "$directory_path" && ! -L "$directory_path" ]]; then
    while IFS= read -r file_path; do
      artifact_record "$file_path" >>"$records_path"
    done < <(find "$directory_path" -type f -print | LC_ALL=C sort)
  fi
  jq -s . "$records_path"
}

write_log_record() {
  local output_name="$1"
  local log_path="$2"
  local passing_pattern="$3"
  local passed_summary="$4"
  local blocked_summary="$5"
  local status="blocked"
  local artifacts='[]'

  if [[ -f "$log_path" && ! -L "$log_path" ]] &&
     rg -q --fixed-strings "$passing_pattern" "$log_path"; then
    status="passed"
    artifacts="$(artifact_record "$log_path" | jq -s .)"
  fi
  jq -n \
    --arg status "$status" \
    --arg summary "$([[ "$status" == "passed" ]] && printf '%s' "$passed_summary" || printf '%s' "$blocked_summary")" \
    --argjson artifacts "$artifacts" \
    '{status: $status, summary: $summary, artifacts: $artifacts}' \
    >"$working_directory/$output_name.json"
}

write_log_record \
  cleanCloneIsolation \
  "$evidence_directory/qualification/clean-clone-isolation.log" \
  "SecondBox clean-clone isolation passed from commit $source_commit" \
  "A clean clone built and validated without SecondStack filesystem reach-through" \
  "Clean-clone evidence for the exact source commit is absent"
write_log_record \
  nonKVMQualification \
  "$evidence_directory/qualification/non-kvm.log" \
  "SecondBox non-KVM qualification passed for commit $source_commit" \
  "The non-KVM contract, test, Compose, image-policy, and build matrix passed" \
  "The non-KVM qualification matrix did not produce passing evidence"

write_qualification_record() {
  local output_name="$1"
  local gate="$2"
  local blocked_summary="$3"
  local record_path="$evidence_directory/qualification/$gate.json"
  local record_relative="qualification/$gate.json"
  local artifacts_path="$working_directory/$output_name-artifacts.jsonl"
  local status="blocked"
  local summary="$blocked_summary"
  local artifacts='[]'

  : >"$artifacts_path"
  if [[ -f "$record_path" && ! -L "$record_path" ]] &&
     node "$repo_root/scripts/verify-release-qualification-record.mjs" \
       "$subject_manifest_path" \
       "$record_path" \
       "$gate" \
       "$evidence_directory"; then
    status="passed"
    summary="$(jq -r '.summary' "$record_path")"
    artifact_record "$record_path" >>"$artifacts_path"
    while IFS= read -r relative_path; do
      [[ -n "$relative_path" ]] || continue
      artifact_record "$evidence_directory/$relative_path" >>"$artifacts_path"
    done < <(jq -r '.scenarios[].artifacts[].path' "$record_path" | LC_ALL=C sort -u)
    artifacts="$(jq -s . "$artifacts_path")"
  fi
  jq -n \
    --arg status "$status" \
    --arg summary "$summary" \
    --arg record "$record_relative" \
    --argjson artifacts "$artifacts" '
      {status: $status, summary: $summary, artifacts: $artifacts} +
      if $status == "passed" then {record: $record} else {} end
    ' >"$working_directory/$output_name.json"
}

write_qualification_record \
  kvmQualification kvm \
  "No complete subject-bound packaged KVM qualification record is present"
write_qualification_record \
  multiRunnerQualification multi-runner \
  "No complete subject-bound two-runner qualification record is present"
write_qualification_record \
  durabilityQualification durability \
  "No complete subject-bound durability qualification record is present"
write_qualification_record \
  dataPlaneQualification data-plane \
  "No complete subject-bound data-plane qualification record is present"
write_qualification_record \
  networkQualification network \
  "No complete subject-bound network-policy qualification record is present"
write_qualification_record \
  securityQualification security \
  "No complete subject-bound security-boundary qualification record is present"

compatibility_artifacts="$(artifact_record "$evidence_directory/current-compatibility.json" | jq -s .)"
compatibility_status="passed"
compatibility_summary="Every compatibility dimension in the canonical manifest is qualified"
if jq -e '
  [
    .publicAPI.releasedClientSkewQualification,
    .runnerProtocol.adjacentGenerationQualification,
    .guestProtocol.priorGenerationQualification,
    .database.upgradeQualification,
    .database.rollingControlPlaneQualification,
    .profiles.schemaQualification,
    .profiles.reachableRevisionUpgradeQualification,
    .checkpoints.qualification,
    .artifacts.qualification
  ] | any(. != "qualified")
' "$evidence_directory/current-compatibility.json" >/dev/null; then
  compatibility_status="blocked"
  compatibility_summary="The canonical manifest explicitly records unqualified or unintegrated compatibility dimensions"
fi
jq -n \
  --arg status "$compatibility_status" \
  --arg summary "$compatibility_summary" \
  --argjson artifacts "$compatibility_artifacts" \
  '{status: $status, summary: $summary, artifacts: $artifacts}' \
  >"$working_directory/compatibilityQualification.json"

write_status_directory_record() {
  local output_name="$1"
  local directory_name="$2"
  local status_file_name="$3"
  local evidence_type="$4"
  local missing_summary="$5"
  local status_file="$evidence_directory/$directory_name/$status_file_name"
  local status="blocked"
  local summary="$missing_summary"
  local artifacts

  artifacts="$(artifacts_in_directory "$evidence_directory/$directory_name")"
  if [[ -f "$status_file" && ! -L "$status_file" ]]; then
    status="$(jq -r '.status // "failed"' "$status_file")"
    summary="$(jq -r '.summary // empty' "$status_file")"
    if [[ -z "$summary" ]]; then
      status="failed"
      summary="$status_file_name has no summary"
    fi
    if ! node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
      "$subject_manifest_path" \
      "$status_file" \
      "$evidence_type" >/dev/null; then
      status="blocked"
      summary="$status_file_name does not cover every exact release subject"
    fi
  fi
  jq -n \
    --arg status "$status" \
    --arg summary "$summary" \
    --argjson artifacts "$artifacts" \
    '{status: $status, summary: $summary, artifacts: $artifacts}' \
    >"$working_directory/$output_name.json"
}

write_status_directory_record \
  sboms sbom sbom-status.json sbom \
  "Required Go, npm, binary, and digest-pinned container SBOM evidence is absent"
write_status_directory_record \
  vulnerabilityReports vulnerabilities vulnerability-status.json vulnerability \
  "Required Go, npm, binary, and container vulnerability evidence is absent"
write_status_directory_record \
  licenses licenses license-status.json license \
  "Required root, Go, npm, container, and execution-asset license evidence is absent"

dependency_age_path="$evidence_directory/dependency-age.json"
dependency_age_status="blocked"
dependency_age_summary="Registry-backed dependency-age evidence is absent"
dependency_age_artifacts='[]'
if [[ -f "$dependency_age_path" && ! -L "$dependency_age_path" ]]; then
  dependency_age_status="$(jq -r '.status // "failed"' "$dependency_age_path")"
  dependency_age_summary="Every pinned Go and npm dependency has registry publication-time evidence and meets the minimum age"
  if [[ "$dependency_age_status" != "passed" ]]; then
    dependency_age_summary="One or more pinned dependencies lack eligible registry publication-time evidence"
  fi
  if ! node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
    "$subject_manifest_path" \
    "$dependency_age_path" \
    dependency-age >/dev/null; then
    dependency_age_status="blocked"
    dependency_age_summary="Dependency-age evidence does not cover every exact release subject"
  fi
  dependency_age_artifacts="$(
    {
      artifact_record "$dependency_age_path"
      if [[ -f "$evidence_directory/dependency-age-inventory.json" &&
            ! -L "$evidence_directory/dependency-age-inventory.json" ]]; then
        artifact_record "$evidence_directory/dependency-age-inventory.json"
      fi
    } | jq -s .
  )"
fi
jq -n \
  --arg status "$dependency_age_status" \
  --arg summary "$dependency_age_summary" \
  --argjson artifacts "$dependency_age_artifacts" \
  '{status: $status, summary: $summary, artifacts: $artifacts}' \
  >"$working_directory/dependencyAge.json"

write_status_directory_record \
  checksums checksums checksum-status.json checksums \
  "Checksums for every exact release subject are absent"

manifest_path="$subject_manifest_path"
manifest_relative="release-subjects.json"
signature_relative="signatures/release-subjects.sig"
public_key_relative="signatures/release-subjects.signing.pub"
signature_status_path="$evidence_directory/signatures/signature-status.json"
signatures_status="blocked"
signatures_summary="A trusted release-key signature over every release subject is absent"
signature_artifacts='[]'
public_key_sha256="0000000000000000000000000000000000000000000000000000000000000000"
if [[ -f "$signature_status_path" &&
      ! -L "$signature_status_path" &&
      -f "$evidence_directory/$signature_relative" &&
      ! -L "$evidence_directory/$signature_relative" &&
      -f "$evidence_directory/$public_key_relative" &&
      ! -L "$evidence_directory/$public_key_relative" ]] &&
   node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
     "$subject_manifest_path" \
     "$signature_status_path" \
     signature >/dev/null &&
   openssl dgst \
     -sha256 \
     -verify "$evidence_directory/$public_key_relative" \
     -signature "$evidence_directory/$signature_relative" \
     "$manifest_path" >/dev/null; then
  signatures_status="passed"
  signatures_summary="The approved release-key signature covers every exact release subject"
  public_key_sha256="$(sha256sum "$evidence_directory/$public_key_relative" | awk '{print $1}')"
  signature_artifacts="$(
    {
      artifact_record "$manifest_path"
      artifact_record "$evidence_directory/$signature_relative"
      artifact_record "$evidence_directory/$public_key_relative"
      artifact_record "$signature_status_path"
    } | jq -s .
  )"
fi
jq -n \
  --arg status "$signatures_status" \
  --arg summary "$signatures_summary" \
  --argjson artifacts "$signature_artifacts" \
  --arg manifest "$manifest_relative" \
  --arg signature "$signature_relative" \
  --arg publicKey "$public_key_relative" \
  --arg publicKeySHA256 "$public_key_sha256" \
  '{
    status: $status, summary: $summary, artifacts: $artifacts,
    manifest: $manifest, signature: $signature, publicKey: $publicKey,
    publicKeySHA256: $publicKeySHA256
  }' >"$working_directory/signatures.json"

provenance_path="$evidence_directory/provenance.intoto.json"
provenance_status_path="$evidence_directory/provenance-status.json"
provenance_status="blocked"
provenance_summary="SLSA-format provenance for every exact release subject is absent"
provenance_artifacts='[]'
if [[ -f "$provenance_path" &&
      ! -L "$provenance_path" &&
      -f "$provenance_status_path" &&
      ! -L "$provenance_status_path" ]] &&
   node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
     "$subject_manifest_path" \
     "$provenance_status_path" \
     provenance >/dev/null; then
  provenance_status="passed"
  provenance_summary="SLSA-format provenance binds every exact release subject to its source commit and builder"
  provenance_artifacts="$(
    {
      artifact_record "$provenance_path"
      artifact_record "$provenance_status_path"
      if [[ -d "$evidence_directory/external-provenance" ]]; then
        while IFS= read -r external_provenance; do
          artifact_record "$external_provenance"
        done < <(find "$evidence_directory/external-provenance" -type f -print | LC_ALL=C sort)
      fi
    } | jq -s .
  )"
fi
jq -n \
  --arg status "$provenance_status" \
  --arg summary "$provenance_summary" \
  --argjson artifacts "$provenance_artifacts" \
  '{status: $status, summary: $summary, artifacts: $artifacts}' \
  >"$working_directory/provenance.json"

jq -n \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" \
  --arg generatedAt "$generated_at" \
  --slurpfile cleanCloneIsolation "$working_directory/cleanCloneIsolation.json" \
  --slurpfile nonKVMQualification "$working_directory/nonKVMQualification.json" \
  --slurpfile kvmQualification "$working_directory/kvmQualification.json" \
  --slurpfile multiRunnerQualification "$working_directory/multiRunnerQualification.json" \
  --slurpfile durabilityQualification "$working_directory/durabilityQualification.json" \
  --slurpfile dataPlaneQualification "$working_directory/dataPlaneQualification.json" \
  --slurpfile networkQualification "$working_directory/networkQualification.json" \
  --slurpfile securityQualification "$working_directory/securityQualification.json" \
  --slurpfile compatibilityQualification "$working_directory/compatibilityQualification.json" \
  --slurpfile sboms "$working_directory/sboms.json" \
  --slurpfile vulnerabilityReports "$working_directory/vulnerabilityReports.json" \
  --slurpfile dependencyAge "$working_directory/dependencyAge.json" \
  --slurpfile licenses "$working_directory/licenses.json" \
  --slurpfile checksums "$working_directory/checksums.json" \
  --slurpfile signatures "$working_directory/signatures.json" \
  --slurpfile provenance "$working_directory/provenance.json" \
  '{
    schemaVersion: 1,
    releaseVersion: $releaseVersion,
    sourceCommit: $sourceCommit,
    generatedAt: $generatedAt,
    compatibility: "current-compatibility.json",
    subjects: "release-subjects.json",
    evidence: {
      cleanCloneIsolation: $cleanCloneIsolation[0],
      nonKVMQualification: $nonKVMQualification[0],
      kvmQualification: $kvmQualification[0],
      multiRunnerQualification: $multiRunnerQualification[0],
      durabilityQualification: $durabilityQualification[0],
      dataPlaneQualification: $dataPlaneQualification[0],
      networkQualification: $networkQualification[0],
      securityQualification: $securityQualification[0],
      compatibilityQualification: $compatibilityQualification[0],
      sboms: $sboms[0],
      vulnerabilityReports: $vulnerabilityReports[0],
      dependencyAge: $dependencyAge[0],
      licenses: $licenses[0],
      checksums: $checksums[0],
      signatures: $signatures[0],
      provenance: $provenance[0]
    }
  }' >"$output_path"

node "$repo_root/scripts/validate-release-evidence.mjs" "$output_path"
cleanup_release_evidence_working_directory
trap - EXIT
echo "$output_path"

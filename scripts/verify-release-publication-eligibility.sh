#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
  echo "Usage: scripts/verify-release-publication-eligibility.sh RELEASE_EVIDENCE.json" >&2
  exit 2
fi

evidence_path="$1"
if [[ -L "$evidence_path" || ! -f "$evidence_path" ]]; then
  echo "SecondBox release publication evidence must be a regular non-symbolic-link file: $evidence_path" >&2
  exit 1
fi
for required_command in jq node openssl realpath sha256sum stat; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox release publication gate requires command: $required_command" >&2
    exit 1
  fi
done
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if ! node "$repo_root/scripts/validate-release-evidence.mjs" "$evidence_path"; then
  echo "SecondBox release publication blocked: evidence does not conform to release/evidence-schema.json" >&2
  exit 1
fi

evidence_directory="$(cd "$(dirname "$evidence_path")" && pwd)"
required_gates=(
  cleanCloneIsolation
  nonKVMQualification
  kvmQualification
  multiRunnerQualification
  durabilityQualification
  dataPlaneQualification
  networkQualification
  securityQualification
  compatibilityQualification
  sboms
  vulnerabilityReports
  dependencyAge
  licenses
  checksums
  signatures
  provenance
)
publication_blocked=false

validate_evidence_relative_file() {
  local label="$1"
  local relative_path="$2"
  local candidate_path
  local canonical_path

  if [[ -z "$relative_path" ||
        "$relative_path" == /* ||
        "$relative_path" == *"//"* ||
        "$relative_path" =~ (^|/)\.\.?(/|$) ]]; then
    echo "SecondBox release publication blocked: unsafe $label path: $relative_path" >&2
    return 1
  fi
  candidate_path="$evidence_directory/$relative_path"
  if [[ -L "$candidate_path" || ! -f "$candidate_path" ]]; then
    echo "SecondBox release publication blocked: $label is unavailable or symbolic: $relative_path" >&2
    return 1
  fi
  if ! canonical_path="$(realpath -e -- "$candidate_path")"; then
    echo "SecondBox release publication blocked: $label cannot be resolved: $relative_path" >&2
    return 1
  fi
  if [[ "$canonical_path" != "$candidate_path" ||
        "$canonical_path" != "$evidence_directory"/* ]]; then
    echo "SecondBox release publication blocked: $label traverses a symbolic link or evidence boundary: $relative_path" >&2
    return 1
  fi
  validated_evidence_file="$canonical_path"
}

for gate in "${required_gates[@]}"; do
  status="$(jq -r --arg gate "$gate" '.evidence[$gate].status // "missing"' "$evidence_path")"
  if [[ "$status" != "passed" ]]; then
    echo "SecondBox release publication blocked: $gate status is $status" >&2
    publication_blocked=true
  fi
  summary="$(jq -r --arg gate "$gate" '.evidence[$gate].summary // empty' "$evidence_path")"
  if [[ -z "$summary" ]]; then
    echo "SecondBox release publication blocked: $gate has no summary" >&2
    publication_blocked=true
  fi
  artifact_count="$(jq -r --arg gate "$gate" '(.evidence[$gate].artifacts // []) | length' "$evidence_path")"
  if [[ "$status" == "passed" && "$artifact_count" -lt 1 ]]; then
    echo "SecondBox release publication blocked: $gate has no artifact evidence" >&2
    publication_blocked=true
  fi

  while IFS=$'\t' read -r artifact_path expected_sha256; do
    [[ -n "$artifact_path" ]] || continue
    if ! validate_evidence_relative_file "$gate artifact" "$artifact_path"; then
      publication_blocked=true
      continue
    fi
    actual_sha256="$(sha256sum "$validated_evidence_file" | awk '{print $1}')"
    if [[ "$actual_sha256" != "$expected_sha256" ]]; then
      echo "SecondBox release publication blocked: $gate artifact checksum mismatch: $artifact_path" >&2
      publication_blocked=true
    fi
  done < <(
    jq -r --arg gate "$gate" '
      (.evidence[$gate].artifacts // [])[] |
      [.path, .sha256] | @tsv
    ' "$evidence_path"
  )
done

source_commit="$(jq -r '.sourceCommit // empty' "$evidence_path")"
release_version="$(jq -r '.releaseVersion // empty' "$evidence_path")"
if [[ ! "$source_commit" =~ ^[0-9a-f]{40}$ ]]; then
  echo "SecondBox release publication blocked: sourceCommit must be 40 lowercase hexadecimal characters" >&2
  publication_blocked=true
fi
compatibility_path="$(jq -r '.compatibility // empty' "$evidence_path")"
if ! validate_evidence_relative_file "compatibility evidence" "$compatibility_path"; then
  publication_blocked=true
elif ! jq -e . "$validated_evidence_file" >/dev/null; then
  echo "SecondBox release publication blocked: compatibility evidence is not valid JSON" >&2
  publication_blocked=true
fi
subject_manifest_path="$(jq -r '.subjects // empty' "$evidence_path")"
if ! validate_evidence_relative_file "release subject manifest" "$subject_manifest_path"; then
  publication_blocked=true
else
  resolved_subject_manifest="$validated_evidence_file"
  if ! jq -e \
    --arg sourceCommit "$source_commit" \
    --arg releaseVersion "$release_version" '
      .releaseVersion == $releaseVersion and
      .sourceCommit == $sourceCommit and
      .status == "passed"
    ' "$resolved_subject_manifest" >/dev/null; then
    echo "SecondBox release publication blocked: release subject manifest candidate identity or status is invalid" >&2
    publication_blocked=true
  fi
  for qualification_specification in \
    "kvmQualification:kvm" \
    "multiRunnerQualification:multi-runner" \
    "durabilityQualification:durability" \
    "dataPlaneQualification:data-plane" \
    "networkQualification:network" \
    "securityQualification:security"; do
    qualification_gate="${qualification_specification%%:*}"
    qualification_record_gate="${qualification_specification#*:}"
    qualification_status="$(
      jq -r --arg gate "$qualification_gate" \
        '.evidence[$gate].status // "missing"' "$evidence_path"
    )"
    if [[ "$qualification_status" != "passed" ]]; then
      continue
    fi
    qualification_record="$(
      jq -r --arg gate "$qualification_gate" \
        '.evidence[$gate].record // empty' "$evidence_path"
    )"
    if ! jq -e \
      --arg gate "$qualification_gate" \
      --arg record "$qualification_record" '
        any(.evidence[$gate].artifacts[]; .path == $record)
      ' "$evidence_path" >/dev/null; then
      echo "SecondBox release publication blocked: $qualification_gate record is not checksum-bound in its artifact list" >&2
      publication_blocked=true
      continue
    fi
    if ! validate_evidence_relative_file \
      "$qualification_gate qualification record" \
      "$qualification_record"; then
      publication_blocked=true
      continue
    fi
    resolved_qualification_record="$validated_evidence_file"
    if ! node "$repo_root/scripts/verify-release-qualification-record.mjs" \
      "$resolved_subject_manifest" \
      "$resolved_qualification_record" \
      "$qualification_record_gate" \
      "$evidence_directory"; then
      echo "SecondBox release publication blocked: $qualification_gate structured record is invalid" >&2
      publication_blocked=true
    fi
  done
  for coverage_specification in \
    "sbom:sbom/sbom-status.json" \
    "vulnerability:vulnerabilities/vulnerability-status.json" \
    "dependency-age:dependency-age.json" \
    "license:licenses/license-status.json" \
    "checksums:checksums/checksum-status.json" \
    "signature:signatures/signature-status.json" \
    "provenance:provenance-status.json"; do
    coverage_type="${coverage_specification%%:*}"
    coverage_path="${coverage_specification#*:}"
    if ! validate_evidence_relative_file "$coverage_type coverage report" "$coverage_path"; then
      publication_blocked=true
      continue
    fi
    if ! node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
      "$resolved_subject_manifest" \
      "$validated_evidence_file" \
      "$coverage_type"; then
      publication_blocked=true
    fi
  done
fi

signature_manifest="$(jq -r '.evidence.signatures.manifest // empty' "$evidence_path")"
signature_path="$(jq -r '.evidence.signatures.signature // empty' "$evidence_path")"
signature_public_key="$(jq -r '.evidence.signatures.publicKey // empty' "$evidence_path")"
signature_public_key_sha256="$(jq -r '.evidence.signatures.publicKeySHA256 // empty' "$evidence_path")"
trusted_public_key_sha256="${SECONDBOX_RELEASE_TRUSTED_PUBLIC_KEY_SHA256-}"
signature_paths_valid=true
if validate_evidence_relative_file "signature manifest" "$signature_manifest"; then
  resolved_signature_manifest="$validated_evidence_file"
else
  publication_blocked=true
  signature_paths_valid=false
fi
if validate_evidence_relative_file "signature" "$signature_path"; then
  resolved_signature="$validated_evidence_file"
else
  publication_blocked=true
  signature_paths_valid=false
fi
if validate_evidence_relative_file "signature public key" "$signature_public_key"; then
  resolved_signature_public_key="$validated_evidence_file"
else
  publication_blocked=true
  signature_paths_valid=false
fi
if [[ "$signature_paths_valid" != true ]]; then
  echo "SecondBox release publication blocked: signed release-subject evidence is incomplete" >&2
elif [[ -z "${resolved_subject_manifest-}" ||
        "$(sha256sum "$resolved_signature_manifest" | awk '{print $1}')" != \
          "$(sha256sum "$resolved_subject_manifest" | awk '{print $1}')" ]]; then
  echo "SecondBox release publication blocked: signed manifest is not the exact canonical release-subject manifest" >&2
  publication_blocked=true
elif [[ ! "$trusted_public_key_sha256" =~ ^[0-9a-f]{64}$ ]]; then
  echo "SecondBox release publication blocked: SECONDBOX_RELEASE_TRUSTED_PUBLIC_KEY_SHA256 must identify the approved release key" >&2
  publication_blocked=true
elif [[ "$signature_public_key_sha256" != "$trusted_public_key_sha256" ||
        "$(sha256sum "$resolved_signature_public_key" | awk '{print $1}')" != "$trusted_public_key_sha256" ]]; then
  echo "SecondBox release publication blocked: signature public key does not match the approved release trust anchor" >&2
  publication_blocked=true
elif ! openssl dgst \
  -sha256 \
  -verify "$resolved_signature_public_key" \
  -signature "$resolved_signature" \
     "$resolved_signature_manifest" >/dev/null; then
  echo "SecondBox release publication blocked: release-subject manifest signature is invalid" >&2
  publication_blocked=true
fi

if [[ "$publication_blocked" == true ]]; then
  exit 1
fi
echo "SecondBox release publication evidence is complete and eligible"

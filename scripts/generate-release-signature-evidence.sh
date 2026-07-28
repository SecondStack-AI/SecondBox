#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  echo "Usage: scripts/generate-release-signature-evidence.sh RELEASE_SUBJECTS.json OUTPUT_DIRECTORY" >&2
  exit 2
fi

subject_manifest="$(realpath -e -- "$1")"
output_directory="$(realpath -e -- "$2")"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_directory="$(dirname "$subject_manifest")"
release_version="${SECONDBOX_RELEASE_VERSION:?set SECONDBOX_RELEASE_VERSION}"
source_commit="${SECONDBOX_RELEASE_SOURCE_COMMIT:?set SECONDBOX_RELEASE_SOURCE_COMMIT}"
private_key="${SECONDBOX_RELEASE_SIGNING_PRIVATE_KEY-}"
trusted_public_key_sha256="${SECONDBOX_RELEASE_TRUSTED_PUBLIC_KEY_SHA256-}"
signature_path="$output_directory/release-subjects.sig"
public_key_path="$output_directory/release-subjects.signing.pub"
status_path="$output_directory/signature-status.json"

if [[ -L "$1" || ! -f "$subject_manifest" || -L "$2" || ! -d "$output_directory" ]]; then
  echo "SecondBox signature evidence requires regular subject-manifest and output inputs" >&2
  exit 1
fi
if [[ "$output_directory" != "$evidence_directory"/* ]]; then
  echo "SecondBox signature output must remain inside the evidence directory" >&2
  exit 1
fi
for output_path in "$signature_path" "$public_key_path" "$status_path"; do
  if [[ -e "$output_path" ]]; then
    echo "SecondBox signature evidence refuses to overwrite: $output_path" >&2
    exit 1
  fi
done
for required_command in jq openssl sha256sum; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox signature evidence requires command: $required_command" >&2
    exit 1
  fi
done

signing_status="blocked"
signing_summary="An approved release signing private key was not supplied"
signature_artifacts='[]'
if [[ -n "$private_key" ]]; then
  if [[ -L "$private_key" || ! -f "$private_key" ]]; then
    echo "SECONDBOX_RELEASE_SIGNING_PRIVATE_KEY must identify a regular non-symbolic-link file" >&2
    exit 1
  fi
  if [[ ! "$trusted_public_key_sha256" =~ ^[0-9a-f]{64}$ ]]; then
    echo "SECONDBOX_RELEASE_TRUSTED_PUBLIC_KEY_SHA256 must identify the independently approved release key" >&2
    exit 1
  fi
  openssl dgst -sha256 -sign "$private_key" -out "$signature_path" "$subject_manifest"
  openssl pkey -in "$private_key" -pubout -out "$public_key_path"
  chmod 0644 "$signature_path" "$public_key_path"
  actual_public_key_sha256="$(sha256sum "$public_key_path" | awk '{print $1}')"
  if [[ "$actual_public_key_sha256" != "$trusted_public_key_sha256" ]]; then
    echo "SecondBox release signing key does not match the independently approved trust anchor" >&2
    exit 1
  fi
  openssl dgst \
    -sha256 \
    -verify "$public_key_path" \
    -signature "$signature_path" \
    "$subject_manifest" >/dev/null
  signing_status="passed"
  signing_summary="The approved release key signs the manifest that binds every exact subject digest"
  signature_artifacts="$(
    for artifact_path in "$subject_manifest" "$signature_path" "$public_key_path"; do
      jq -cn \
        --arg path "${artifact_path#"$evidence_directory"/}" \
        --arg sha256 "$(sha256sum "$artifact_path" | awk '{print $1}')" \
        '{path: $path, sha256: $sha256}'
    done | jq -s .
  )"
fi

working_directory="$(mktemp -d)"
cleanup_release_signature_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox signature evidence failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_signature_working_directory EXIT
subject_records="$working_directory/subjects.jsonl"
: >"$subject_records"
while IFS= read -r subject; do
  subject_id="$(jq -r '.id' <<<"$subject")"
  subject_status="$(jq -r '.status' <<<"$subject")"
  subject_sha256="$(jq -r '.digest.sha256 // "0000000000000000000000000000000000000000000000000000000000000000"' <<<"$subject")"
  record_status="$signing_status"
  record_summary="$signing_summary"
  if [[ "$subject_status" != "passed" ]]; then
    record_status="blocked"
    record_summary="The exact release subject is unavailable and therefore cannot be signed"
  fi
  jq -cn \
    --arg subjectId "$subject_id" \
    --arg subjectSHA256 "$subject_sha256" \
    --arg status "$record_status" \
    --arg summary "$record_summary" \
    --argjson artifacts "$signature_artifacts" \
    '{
      subjectId: $subjectId,
      subjectSHA256: $subjectSHA256,
      status: $status,
      summary: $summary,
      artifacts: $artifacts
    }' >>"$subject_records"
done < <(jq -c '.subjects[]' "$subject_manifest")

overall_status="$signing_status"
if [[ "$(jq -r '.status' "$subject_manifest")" != "passed" ]]; then
  overall_status="blocked"
fi
jq -s \
  --arg status "$overall_status" \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" '
    {
      schemaVersion: 1,
      evidenceType: "signature",
      releaseVersion: $releaseVersion,
      sourceCommit: $sourceCommit,
      status: $status,
      summary: (
        if $status == "passed" then
          "The approved release key signs the manifest for every exact release subject"
        else
          "An approved signature over every exact release subject is unavailable"
        end
      ),
      subjects: .
    }
  ' "$subject_records" >"$status_path"

node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
  "$subject_manifest" \
  "$status_path" \
  signature
cleanup_release_signature_working_directory
trap - EXIT

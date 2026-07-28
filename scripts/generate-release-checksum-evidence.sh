#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  echo "Usage: scripts/generate-release-checksum-evidence.sh RELEASE_SUBJECTS.json OUTPUT_DIRECTORY" >&2
  exit 2
fi

subject_manifest="$(realpath -e -- "$1")"
output_directory="$(realpath -e -- "$2")"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_directory="$(dirname "$subject_manifest")"
release_version="${SECONDBOX_RELEASE_VERSION:?set SECONDBOX_RELEASE_VERSION}"
source_commit="${SECONDBOX_RELEASE_SOURCE_COMMIT:?set SECONDBOX_RELEASE_SOURCE_COMMIT}"

if [[ -L "$1" || ! -f "$subject_manifest" || -L "$2" || ! -d "$output_directory" ]]; then
  echo "SecondBox checksum evidence requires regular subject-manifest and output inputs" >&2
  exit 1
fi
if [[ "$output_directory" != "$evidence_directory"/* ]]; then
  echo "SecondBox checksum output must remain inside the evidence directory" >&2
  exit 1
fi
for required_command in jq realpath sha256sum; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox checksum evidence requires command: $required_command" >&2
    exit 1
  fi
done

checksum_inventory="$output_directory/release-subject-checksums.json"
local_checksum_file="$output_directory/release-subjects.SHA256SUMS"
status_path="$output_directory/checksum-status.json"
for output_path in "$checksum_inventory" "$local_checksum_file" "$status_path"; do
  if [[ -e "$output_path" ]]; then
    echo "SecondBox checksum evidence refuses to overwrite: $output_path" >&2
    exit 1
  fi
done

working_directory="$(mktemp -d)"
cleanup_release_checksum_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox checksum evidence failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_checksum_working_directory EXIT
inventory_records="$working_directory/inventory.jsonl"
subject_records="$working_directory/subjects.jsonl"
: >"$inventory_records"
: >"$subject_records"
: >"$local_checksum_file"

inventory_artifact() {
  local artifact_path="$1"
  jq -cn \
    --arg path "${artifact_path#"$evidence_directory"/}" \
    --arg sha256 "$(sha256sum "$artifact_path" | awk '{print $1}')" \
    '{path: $path, sha256: $sha256}'
}

while IFS= read -r subject; do
  subject_id="$(jq -r '.id' <<<"$subject")"
  subject_status="$(jq -r '.status' <<<"$subject")"
  subject_sha256="$(jq -r '.digest.sha256 // "0000000000000000000000000000000000000000000000000000000000000000"' <<<"$subject")"
  locator="$(jq -r '.locator // empty' <<<"$subject")"
  record_status="blocked"
  record_summary="The exact release subject is unavailable"
  if [[ "$subject_status" == "passed" && "$locator" == *@sha256:* ]]; then
    record_status="passed"
    record_summary="The registry-backed OCI locator embeds the exact subject digest"
    jq -cn \
      --arg subjectId "$subject_id" \
      --arg locator "$locator" \
      --arg sha256 "$subject_sha256" \
      '{subjectId: $subjectId, locator: $locator, sha256: $sha256, locationType: "oci"}' \
      >>"$inventory_records"
  elif [[ "$subject_status" == "passed" ]]; then
    subject_path="$evidence_directory/$locator"
    if [[ -L "$subject_path" || ! -f "$subject_path" ]]; then
      record_status="failed"
      record_summary="The subject manifest locator is unavailable or symbolic"
    elif [[ "$(realpath -e -- "$subject_path")" != "$subject_path" ]] ||
         [[ "$subject_path" != "$evidence_directory"/* ]]; then
      record_status="failed"
      record_summary="The subject manifest locator escapes the evidence directory"
    elif [[ "$(sha256sum "$subject_path" | awk '{print $1}')" != "$subject_sha256" ]]; then
      record_status="failed"
      record_summary="The local subject checksum does not match the subject manifest"
    else
      record_status="passed"
      record_summary="The local release subject checksum matches the exact candidate bytes"
      printf '%s  %s\n' "$subject_sha256" "$locator" >>"$local_checksum_file"
      jq -cn \
        --arg subjectId "$subject_id" \
        --arg locator "$locator" \
        --arg sha256 "$subject_sha256" \
        '{subjectId: $subjectId, locator: $locator, sha256: $sha256, locationType: "file"}' \
        >>"$inventory_records"
    fi
  fi
  jq -cn \
    --arg subjectId "$subject_id" \
    --arg subjectSHA256 "$subject_sha256" \
    --arg status "$record_status" \
    --arg summary "$record_summary" \
    '{
      subjectId: $subjectId,
      subjectSHA256: $subjectSHA256,
      status: $status,
      summary: $summary,
      artifacts: []
    }' >>"$subject_records"
done < <(jq -c '.subjects[]' "$subject_manifest")

jq -s \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" '
    {
      schemaVersion: 1,
      releaseVersion: $releaseVersion,
      sourceCommit: $sourceCommit,
      subjects: .
    }
  ' "$inventory_records" >"$checksum_inventory"

inventory_artifacts="$(
  {
    inventory_artifact "$checksum_inventory"
    inventory_artifact "$local_checksum_file"
  } | jq -s .
)"
jq -c --argjson artifacts "$inventory_artifacts" \
  '.artifacts = $artifacts' "$subject_records" >"$working_directory/subjects-with-artifacts.jsonl"
overall_status="passed"
if jq -e 'select(.status == "failed")' "$working_directory/subjects-with-artifacts.jsonl" >/dev/null; then
  overall_status="failed"
elif [[ "$(jq -r '.status' "$subject_manifest")" != "passed" ]] ||
     jq -e 'select(.status != "passed")' "$working_directory/subjects-with-artifacts.jsonl" >/dev/null; then
  overall_status="blocked"
fi
jq -s \
  --arg status "$overall_status" \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" '
    {
      schemaVersion: 1,
      evidenceType: "checksums",
      releaseVersion: $releaseVersion,
      sourceCommit: $sourceCommit,
      status: $status,
      summary: (
        if $status == "passed" then
          "Every local candidate byte and registry-backed image is bound to the exact release subject digest"
        elif $status == "failed" then
          "One or more release-subject checksums does not match the candidate"
        else
          "One or more exact release subjects is unavailable for checksum evidence"
        end
      ),
      subjects: .
    }
  ' "$working_directory/subjects-with-artifacts.jsonl" >"$status_path"

node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
  "$subject_manifest" \
  "$status_path" \
  checksums
cleanup_release_checksum_working_directory
trap - EXIT

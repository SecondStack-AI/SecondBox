#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  echo "Usage: scripts/generate-release-provenance.sh RELEASE_SUBJECTS.json OUTPUT.intoto.json" >&2
  exit 2
fi

subject_manifest="$(realpath -e -- "$1")"
output_path="$2"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_directory="$(dirname "$subject_manifest")"
status_path="$evidence_directory/provenance-status.json"
release_version="${SECONDBOX_RELEASE_VERSION:?set SECONDBOX_RELEASE_VERSION}"
source_commit="${SECONDBOX_RELEASE_SOURCE_COMMIT:?set SECONDBOX_RELEASE_SOURCE_COMMIT}"
generated_at="${SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP:?set SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP}"
builder_identity="${SECONDBOX_RELEASE_BUILDER_IDENTITY:?set SECONDBOX_RELEASE_BUILDER_IDENTITY}"

if [[ -L "$1" || ! -f "$subject_manifest" ]]; then
  echo "SecondBox provenance requires a regular non-symbolic-link subject manifest" >&2
  exit 1
fi
if [[ "$(dirname "$(realpath -m -- "$output_path")")" != "$evidence_directory" ]]; then
  echo "SecondBox provenance output must be written at the evidence root" >&2
  exit 1
fi
for candidate_output in "$output_path" "$status_path"; do
  if [[ -e "$candidate_output" ]]; then
    echo "SecondBox provenance refuses to overwrite: $candidate_output" >&2
    exit 1
  fi
done
if [[ ! "$source_commit" =~ ^[0-9a-f]{40}$ ]] ||
   [[ "$(git -C "$repo_root" rev-parse HEAD)" != "$source_commit" ]]; then
  echo "SecondBox provenance source commit must equal the checked-out commit" >&2
  exit 1
fi
if [[ -n "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" ]]; then
  echo "SecondBox release provenance requires a clean source tree" >&2
  exit 1
fi
for required_command in git jq realpath sha256sum; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox provenance requires command: $required_command" >&2
    exit 1
  fi
done

working_directory="$(mktemp -d)"
cleanup_release_provenance_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox provenance failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_provenance_working_directory EXIT
materials_path="$working_directory/materials.jsonl"
local_subjects_path="$working_directory/local-subjects.jsonl"
subject_records="$working_directory/subject-records.jsonl"
: >"$materials_path"
: >"$local_subjects_path"
: >"$subject_records"

for material in \
  go.mod \
  go.sum \
  runner/go.mod \
  runner/go.sum \
  package-lock.json \
  Dockerfile \
  runner/Dockerfile \
  runner/deploy/microvm-artifact-transport.Dockerfile \
  scripts/build-artifacts.sh \
  scripts/package-release-artifacts.sh \
  scripts/package-release-sdk-artifacts.sh \
  scripts/package-release-guest-assets.sh \
  release/current-compatibility.json \
  release/supply-chain-subjects-schema.json; do
  jq -cn \
    --arg uri "git+https://github.com/SecondStack-AI/SecondBox@$source_commit#$material" \
    --arg sha256 "$(sha256sum "$repo_root/$material" | awk '{print $1}')" \
    '{uri: $uri, digest: {sha256: $sha256}}' >>"$materials_path"
done
if [[ -f "$evidence_directory/tools/release-evidence-tools.json" &&
      ! -L "$evidence_directory/tools/release-evidence-tools.json" ]]; then
  jq -cn \
    --arg uri "secondbox-evidence:tools/release-evidence-tools.json" \
    --arg sha256 "$(sha256sum "$evidence_directory/tools/release-evidence-tools.json" | awk '{print $1}')" \
    '{uri: $uri, digest: {sha256: $sha256}}' >>"$materials_path"
fi

for subject_id in \
  linux-release-package \
  secondbox \
  secondbox-artifact-evidence \
  secondbox-guest-agent \
  secondbox-runner \
  secondbox-runner-identity \
  secondboxd \
  go-sdk-package \
  typescript-sdk-package; do
  jq -c --arg subject_id "$subject_id" '
    .subjects[] |
    select(.id == $subject_id and .status == "passed") |
    {name: .locator, digest: .digest}
  ' "$subject_manifest" >>"$local_subjects_path"
done

jq -n \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" \
  --arg generatedAt "$generated_at" \
  --arg builderIdentity "$builder_identity" \
  --argjson subjects "$(jq -s . "$local_subjects_path")" \
  --argjson materials "$(jq -s . "$materials_path")" '
    {
      _type: "https://in-toto.io/Statement/v1",
      subject: $subjects,
      predicateType: "https://slsa.dev/provenance/v1",
      predicate: {
        buildDefinition: {
          buildType: "https://secondbox.dev/build-types/release-local-subjects/v1",
          externalParameters: {
            releaseVersion: $releaseVersion,
            sourceCommit: $sourceCommit
          },
          internalParameters: {},
          resolvedDependencies: $materials
        },
        runDetails: {
          builder: {id: $builderIdentity},
          metadata: {
            invocationId: ($sourceCommit + ":" + $releaseVersion),
            startedOn: $generatedAt,
            finishedOn: $generatedAt
          }
        }
      }
    }
  ' >"$output_path"

local_provenance_artifact="$(
  jq -cn \
    --arg path "${output_path#"$evidence_directory"/}" \
    --arg sha256 "$(sha256sum "$output_path" | awk '{print $1}')" \
    '{path: $path, sha256: $sha256}' |
    jq -s .
)"
for subject_id in \
  linux-release-package \
  secondbox \
  secondbox-artifact-evidence \
  secondbox-guest-agent \
  secondbox-runner \
  secondbox-runner-identity \
  secondboxd \
  go-sdk-package \
  typescript-sdk-package; do
  subject_status="$(jq -r --arg subject_id "$subject_id" '.subjects[] | select(.id == $subject_id) | .status' "$subject_manifest")"
  subject_sha256="$(jq -r --arg subject_id "$subject_id" '.subjects[] | select(.id == $subject_id) | .digest.sha256 // "0000000000000000000000000000000000000000000000000000000000000000"' "$subject_manifest")"
  record_status="passed"
  record_summary="SLSA v1 provenance binds the exact locally built subject to the source commit and locked materials"
  artifacts="$local_provenance_artifact"
  if [[ "$subject_status" != "passed" ]]; then
    record_status="blocked"
    record_summary="The exact local release subject is unavailable"
    artifacts='[]'
  fi
  jq -cn \
    --arg subjectId "$subject_id" \
    --arg subjectSHA256 "$subject_sha256" \
    --arg status "$record_status" \
    --arg summary "$record_summary" \
    --argjson artifacts "$artifacts" \
    '{
      subjectId: $subjectId,
      subjectSHA256: $subjectSHA256,
      status: $status,
      summary: $summary,
      artifacts: $artifacts
    }' >>"$subject_records"
done

external_provenance_directory="$evidence_directory/external-provenance"
install -d -m 0755 "$external_provenance_directory"
verify_external_provenance() {
  local subject_id="$1"
  local environment_name="$2"
  local bound_subject_id="${3-}"
  local supplied_path="${!environment_name-}"
  local subject_status
  local subject_sha256
  local bound_subject_sha256=""
  local copied_path
  local record_status="blocked"
  local record_summary="$environment_name was not supplied"
  local artifacts='[]'

  subject_status="$(jq -r --arg subject_id "$subject_id" '.subjects[] | select(.id == $subject_id) | .status' "$subject_manifest")"
  subject_sha256="$(jq -r --arg subject_id "$subject_id" '.subjects[] | select(.id == $subject_id) | .digest.sha256 // "0000000000000000000000000000000000000000000000000000000000000000"' "$subject_manifest")"
  if [[ -n "$bound_subject_id" ]]; then
    bound_subject_sha256="$(
      jq -r --arg subject_id "$bound_subject_id" '
        .subjects[] |
        select(.id == $subject_id and .status == "passed") |
        .digest.sha256 // empty
      ' "$subject_manifest"
    )"
  fi
  if [[ "$subject_status" != "passed" ]]; then
    record_summary="The exact externally built release subject is unavailable"
  elif [[ -n "$supplied_path" ]]; then
    if [[ -L "$supplied_path" || ! -f "$supplied_path" ]]; then
      record_status="failed"
      record_summary="$environment_name is unavailable or symbolic"
    elif ! jq -e \
      --arg sourceCommit "$source_commit" \
      --arg subjectSHA256 "$subject_sha256" \
      --arg boundSubjectSHA256 "$bound_subject_sha256" '
        ._type == "https://in-toto.io/Statement/v1" and
        .predicateType == "https://slsa.dev/provenance/v1" and
        any(.subject[]; .digest.sha256 == $subjectSHA256) and
        (
          $boundSubjectSHA256 == "" or
          any(
            .predicate.buildDefinition.resolvedDependencies[];
            .digest.sha256 == $boundSubjectSHA256
          )
        ) and
        (
          .predicate.buildDefinition.externalParameters.sourceCommit == $sourceCommit or
          any(
            .predicate.buildDefinition.resolvedDependencies[];
            (.uri | contains($sourceCommit))
          )
        )
      ' "$supplied_path" >/dev/null; then
      record_status="failed"
      record_summary="$environment_name does not bind the exact subject digest, source commit, and required subject materials"
    else
      copied_path="$external_provenance_directory/$subject_id.intoto.json"
      install -m 0644 "$supplied_path" "$copied_path"
      record_status="passed"
      record_summary="Externally produced SLSA v1 provenance binds the exact subject digest and source commit"
      artifacts="$(
        jq -cn \
          --arg path "${copied_path#"$evidence_directory"/}" \
          --arg sha256 "$(sha256sum "$copied_path" | awk '{print $1}')" \
          '{path: $path, sha256: $sha256}' |
          jq -s .
      )"
    fi
  fi
  jq -cn \
    --arg subjectId "$subject_id" \
    --arg subjectSHA256 "$subject_sha256" \
    --arg status "$record_status" \
    --arg summary "$record_summary" \
    --argjson artifacts "$artifacts" \
    '{
      subjectId: $subjectId,
      subjectSHA256: $subjectSHA256,
      status: $status,
      summary: $summary,
      artifacts: $artifacts
    }' >>"$subject_records"
}

verify_external_provenance \
  control-plane-image \
  SECONDBOX_RELEASE_CONTROL_PLANE_IMAGE_PROVENANCE
verify_external_provenance \
  runner-image \
  SECONDBOX_RELEASE_RUNNER_IMAGE_PROVENANCE
verify_external_provenance \
  guest-execution-bundle \
  SECONDBOX_RELEASE_GUEST_BUNDLE_PROVENANCE
verify_external_provenance \
  guest-artifact-image \
  SECONDBOX_RELEASE_GUEST_ARTIFACT_IMAGE_PROVENANCE \
  guest-execution-bundle

overall_status="passed"
if jq -e 'select(.status == "failed")' "$subject_records" >/dev/null; then
  overall_status="failed"
elif [[ "$(jq -r '.status' "$subject_manifest")" != "passed" ]] ||
     jq -e 'select(.status != "passed")' "$subject_records" >/dev/null; then
  overall_status="blocked"
fi
jq -s \
  --arg status "$overall_status" \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" '
    {
      schemaVersion: 1,
      evidenceType: "provenance",
      releaseVersion: $releaseVersion,
      sourceCommit: $sourceCommit,
      status: $status,
      summary: (
        if $status == "passed" then
          "Every exact release subject has SLSA v1 provenance bound to its digest and source commit"
        elif $status == "failed" then
          "One or more supplied provenance statements does not bind the exact candidate"
        else
          "External image or guest-bundle provenance is unavailable"
        end
      ),
      subjects: .
    }
  ' "$subject_records" >"$status_path"

node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
  "$subject_manifest" \
  "$status_path" \
  provenance
cleanup_release_provenance_working_directory
trap - EXIT

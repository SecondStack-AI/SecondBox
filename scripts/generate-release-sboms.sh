#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 3 ]]; then
  echo "Usage: scripts/generate-release-sboms.sh OUTPUT_DIRECTORY BINARY_DIRECTORY RELEASE_SUBJECTS.json" >&2
  exit 2
fi

output_directory="$(realpath -e -- "$1")"
binary_directory="$(realpath -e -- "$2")"
subject_manifest="$(realpath -e -- "$3")"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_directory="$(dirname "$subject_manifest")"
generated_at="${SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP:?set SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP}"
release_version="${SECONDBOX_RELEASE_VERSION:?set SECONDBOX_RELEASE_VERSION}"
source_commit="${SECONDBOX_RELEASE_SOURCE_COMMIT:?set SECONDBOX_RELEASE_SOURCE_COMMIT}"

if [[ -L "$1" || ! -d "$output_directory" ||
      -L "$2" || ! -d "$binary_directory" ||
      -L "$3" || ! -f "$subject_manifest" ]]; then
  echo "SecondBox SBOM generation requires regular output, binary, and subject-manifest inputs" >&2
  exit 1
fi
if [[ "$output_directory" != "$evidence_directory"/* ||
      "$binary_directory" != "$evidence_directory"/* ]]; then
  echo "SecondBox SBOM inputs and output must remain inside one evidence directory" >&2
  exit 1
fi
for required_command in go jq npm sha256sum tar; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox SBOM generation requires command: $required_command" >&2
    exit 1
  fi
done

working_directory="$(mktemp -d)"
cleanup_release_sbom_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox SBOM generation failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_sbom_working_directory EXIT
subject_records="$working_directory/subject-records.jsonl"
: >"$subject_records"

evidence_artifact() {
  local artifact_path="$1"
  local canonical_path
  local relative_path

  if [[ -L "$artifact_path" || ! -f "$artifact_path" ]]; then
    echo "SecondBox SBOM artifact must be a regular non-symbolic-link file: $artifact_path" >&2
    return 1
  fi
  canonical_path="$(realpath -e -- "$artifact_path")"
  if [[ "$canonical_path" != "$evidence_directory"/* ]]; then
    echo "SecondBox SBOM artifact escapes the evidence directory: $artifact_path" >&2
    return 1
  fi
  relative_path="${canonical_path#"$evidence_directory"/}"
  jq -cn \
    --arg path "$relative_path" \
    --arg sha256 "$(sha256sum "$canonical_path" | awk '{print $1}')" \
    '{path: $path, sha256: $sha256}'
}

subject_digest() {
  jq -r --arg subject_id "$1" '
    .subjects[] |
    select(.id == $subject_id) |
    .digest.sha256 // "0000000000000000000000000000000000000000000000000000000000000000"
  ' "$subject_manifest"
}

subject_locator() {
  jq -r --arg subject_id "$1" '
    .subjects[] |
    select(.id == $subject_id) |
    .locator // empty
  ' "$subject_manifest"
}

subject_candidate_status() {
  jq -r --arg subject_id "$1" '
    .subjects[] |
    select(.id == $subject_id) |
    .status
  ' "$subject_manifest"
}

record_subject_sbom() {
  local subject_id="$1"
  local status="$2"
  local summary="$3"
  shift 3
  local artifact_json='[]'
  if [[ "$#" -gt 0 ]]; then
    artifact_json="$(
      for artifact_path in "$@"; do
        evidence_artifact "$artifact_path"
      done | jq -s .
    )"
  fi
  jq -cn \
    --arg subjectId "$subject_id" \
    --arg subjectSHA256 "$(subject_digest "$subject_id")" \
    --arg status "$status" \
    --arg summary "$summary" \
    --argjson artifacts "$artifact_json" \
    '{
      subjectId: $subjectId,
      subjectSHA256: $subjectSHA256,
      status: $status,
      summary: $summary,
      artifacts: $artifacts
    }' >>"$subject_records"
}

generate_go_module_sbom() {
  local module_directory="$1"
  local component_name="$2"
  local output_path="$3"
  local module_json="$working_directory/$(basename "$output_path").modules.json"

  GOWORK=off go -C "$module_directory" list -m -json all | jq -s . >"$module_json"
  jq \
    --arg timestamp "$generated_at" \
    --arg component "$component_name" \
    --arg version "$release_version" '
      {
        bomFormat: "CycloneDX",
        specVersion: "1.6",
        version: 1,
        metadata: {
          timestamp: $timestamp,
          component: {
            type: "application",
            name: $component,
            version: $version
          },
          tools: {
            components: [{
              type: "application",
              name: "go list",
              version: "Go module graph"
            }]
          }
        },
        components: [
          .[] |
          select(.Main != true and .Version != null) |
          {
            type: "library",
            name: .Path,
            version: .Version,
            purl: ("pkg:golang/" + .Path + "@" + .Version)
          }
        ] | unique_by(.purl)
      }
    ' "$module_json" >"$output_path"
}

generate_go_module_sbom \
  "$repo_root" \
  "secondbox-control-plane-and-go-sdk" \
  "$output_directory/control-plane-go-modules.cdx.json"
generate_go_module_sbom \
  "$repo_root/runner" \
  "secondbox-runner-and-guest-agent" \
  "$output_directory/runner-go-modules.cdx.json"
(
  cd "$repo_root"
  npm sbom \
    --package-lock-only \
    --sbom-format cyclonedx \
    --sbom-type application >"$output_directory/npm-lock.cdx.json"
)

generate_guest_bundle_sbom() {
  local guest_archive="$1"
  local output_path="$2"
  local guest_extract="$working_directory/guest"
  local guest_root

  if tar -tzf "$guest_archive" |
    awk '
      /^\\// {bad=1}
      /(^|\\/)\\.\\.($|\\/)/ {bad=1}
      END {exit bad}
    '; then
    :
  else
    echo "SecondBox guest SBOM input contains an unsafe archive path" >&2
    return 1
  fi
  install -d -m 0755 "$guest_extract"
  tar --extract --gzip --file "$guest_archive" --directory "$guest_extract" --no-same-owner
  if find "$guest_extract" -type l -print -quit | grep -q .; then
    echo "SecondBox guest SBOM input contains a symbolic link" >&2
    return 1
  fi
  guest_root="$(find "$guest_extract" -mindepth 1 -maxdepth 1 -type d -print -quit)"
  if [[ -z "$guest_root" ||
        ! -f "$guest_root/manifest.json" ||
        ! -f "$guest_root/rootfs-debian-license-inventory.json" ||
        ! -f "$guest_root/rootfs-python-license-inventory.json" ]]; then
    echo "SecondBox guest SBOM input lacks its signed inventories" >&2
    return 1
  fi
  jq -n \
    --arg timestamp "$generated_at" \
    --arg version "$release_version" \
    --arg subjectSHA256 "$(sha256sum "$guest_archive" | awk '{print $1}')" \
    --slurpfile manifest "$guest_root/manifest.json" \
    --slurpfile debian "$guest_root/rootfs-debian-license-inventory.json" \
    --slurpfile python "$guest_root/rootfs-python-license-inventory.json" '
      {
        bomFormat: "CycloneDX",
        specVersion: "1.6",
        version: 1,
        metadata: {
          timestamp: $timestamp,
          component: {
            type: "operating-system",
            name: "secondbox-guest-execution-bundle",
            version: $version,
            hashes: [{alg: "SHA-256", content: $subjectSHA256}]
          },
          tools: {
            components: [{
              type: "application",
              name: "SecondBox signed guest inventory converter",
              version: "1"
            }]
          }
        },
        components:
          (
            [
              {
                type: "file",
                name: "kernel",
                hashes: [{alg: "SHA-256", content: $manifest[0].kernel.sha256}]
              },
              {
                type: "file",
                name: "rootfs.ext4",
                hashes: [{alg: "SHA-256", content: $manifest[0].rootfs.sha256}]
              },
              {
                type: "file",
                name: "shared.img",
                hashes: [{alg: "SHA-256", content: $manifest[0].shared.sha256}]
              }
            ] +
            [
              $debian[0].packages[] |
              {
                type: "library",
                name: .package,
                version: .version,
                purl: (
                  "pkg:deb/debian/" +
                  (.package | @uri) +
                  "@" +
                  (.version | @uri) +
                  "?arch=" +
                  (.architecture | @uri)
                ),
                properties: [
                  {name: "secondbox:debian:source", value: .source},
                  {name: "secondbox:debian:copyright-sha256", value: .copyrightSha256}
                ]
              }
            ] +
            [
              $python[0].distributions[] |
              {
                type: "library",
                name: .name,
                version: .version,
                purl: ("pkg:pypi/" + (.name | ascii_downcase | @uri) + "@" + (.version | @uri)),
                licenses: (
                  if .licenseExpression != "" then
                    [{expression: .licenseExpression}]
                  elif .license != "" then
                    [{license: {name: .license}}]
                  else
                    []
                  end
                )
              }
            ]
          )
      }
    ' >"$output_path"
}

if [[ "$(subject_candidate_status guest-execution-bundle)" == "passed" ]]; then
  guest_locator="$(subject_locator guest-execution-bundle)"
  guest_archive="$evidence_directory/$guest_locator"
  if generate_guest_bundle_sbom \
    "$guest_archive" \
    "$output_directory/guest-execution-bundle.cdx.json"; then
    record_subject_sbom \
      guest-execution-bundle \
      passed \
      "CycloneDX SBOM binds the signed guest kernel, filesystem, tool image, Debian packages, and Python distributions" \
      "$output_directory/guest-execution-bundle.cdx.json"
  else
    record_subject_sbom \
      guest-execution-bundle \
      failed \
      "The signed guest bundle could not be converted into a complete CycloneDX SBOM"
  fi
else
  record_subject_sbom \
    guest-execution-bundle \
    blocked \
    "The exact signed guest execution bundle is unavailable"
fi

syft_available=false
tool_manifest="$evidence_directory/tools/release-evidence-tools.json"
expected_syft="$evidence_directory/tools/syft"
syft_command="$(command -v syft 2>/dev/null || true)"
if [[ -f "$tool_manifest" &&
      ! -L "$tool_manifest" &&
      -x "$expected_syft" &&
      ! -L "$expected_syft" &&
      -n "$syft_command" ]] &&
   [[ "$(realpath -e -- "$syft_command")" == "$expected_syft" ]] &&
   [[ "$(sha256sum "$expected_syft" | awk '{print $1}')" == "$(jq -r '.tools[] | select(.name == "syft") | .installedBinarySHA256' "$tool_manifest")" ]]; then
  syft_available=true
fi
for subject_id in \
  linux-release-package \
  secondbox \
  secondbox-artifact-evidence \
  secondbox-guest-agent \
  secondbox-runner \
  secondbox-runner-identity \
  secondboxd \
  control-plane-image \
  runner-image \
  guest-artifact-image \
  go-sdk-package \
  typescript-sdk-package; do
  if [[ "$(subject_candidate_status "$subject_id")" != "passed" ]]; then
    record_subject_sbom "$subject_id" blocked "The exact release subject is unavailable"
    continue
  fi
  if [[ "$syft_available" != true ]]; then
    record_subject_sbom "$subject_id" blocked "Syft is unavailable for exact-subject SBOM generation"
    continue
  fi
  locator="$(subject_locator "$subject_id")"
  output_path="$output_directory/$subject_id.cdx.json"
  if [[ "$locator" == *@sha256:* ]]; then
    syft_target="$locator"
  else
    syft_target="file:$evidence_directory/$locator"
  fi
  if syft "$syft_target" -o "cyclonedx-json=$output_path"; then
    record_subject_sbom \
      "$subject_id" \
      passed \
      "Syft generated a CycloneDX SBOM for the exact subject digest" \
      "$output_path" \
      "$tool_manifest"
  else
    record_subject_sbom \
      "$subject_id" \
      failed \
      "Syft failed to generate an SBOM for the exact subject digest"
  fi
done

overall_status="passed"
if [[ "$(jq -r '.status' "$subject_manifest")" != "passed" ]] ||
   jq -e 'select(.status != "passed")' "$subject_records" >/dev/null; then
  overall_status="blocked"
fi
jq -s \
  --arg status "$overall_status" \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" '
    {
      schemaVersion: 1,
      evidenceType: "sbom",
      releaseVersion: $releaseVersion,
      sourceCommit: $sourceCommit,
      status: $status,
      summary: (
        if $status == "passed" then
          "Every exact release image, binary, guest asset bundle, and SDK package has a subject-bound CycloneDX SBOM"
        else
          "One or more exact release subjects lack a complete subject-bound SBOM"
        end
      ),
      subjects: .
    }
  ' "$subject_records" >"$output_directory/sbom-status.json"

node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
  "$subject_manifest" \
  "$output_directory/sbom-status.json" \
  sbom
cleanup_release_sbom_working_directory
trap - EXIT

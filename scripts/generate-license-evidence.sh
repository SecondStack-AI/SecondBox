#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  echo "Usage: scripts/generate-license-evidence.sh OUTPUT_DIRECTORY RELEASE_SUBJECTS.json" >&2
  exit 2
fi

output_directory="$(realpath -e -- "$1")"
subject_manifest="$(realpath -e -- "$2")"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_directory="$(dirname "$subject_manifest")"
release_version="${SECONDBOX_RELEASE_VERSION:?set SECONDBOX_RELEASE_VERSION}"
source_commit="${SECONDBOX_RELEASE_SOURCE_COMMIT:?set SECONDBOX_RELEASE_SOURCE_COMMIT}"

if [[ -L "$1" || ! -d "$output_directory" || -L "$2" || ! -f "$subject_manifest" ]]; then
  echo "SecondBox license evidence requires regular output and subject-manifest inputs" >&2
  exit 1
fi
if [[ "$output_directory" != "$evidence_directory"/* ]]; then
  echo "SecondBox license output must remain inside the evidence directory" >&2
  exit 1
fi
for required_command in find go jq sha256sum tar; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox license evidence requires command: $required_command" >&2
    exit 1
  fi
done

install -m 0644 "$repo_root/LICENSE" "$output_directory/LICENSE"
install -m 0644 "$repo_root/THIRD_PARTY_NOTICES.md" "$output_directory/THIRD_PARTY_NOTICES.md"
install -m 0644 \
  "$repo_root/sdk/typescript/flue-runtime-beta9-LICENSE.txt" \
  "$output_directory/FLUE_RUNTIME_BETA9_LICENSE.txt"
install -m 0644 \
  "$repo_root/sdk/typescript/flue-runtime-beta9-source.json" \
  "$output_directory/flue-runtime-beta9-source.json"
jq '
  .packages |
  to_entries |
  map(
    select(.key | test("(^|/)node_modules/")) |
    {
      path: .key,
      version: .value.version,
      license: (.value.license // "UNKNOWN"),
      resolved: (.value.resolved // null),
      integrity: (.value.integrity // null)
    }
  )
' "$repo_root/package-lock.json" >"$output_directory/npm-license-inventory.json"

working_directory="$(mktemp -d)"
cleanup_license_evidence_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox license evidence failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_license_evidence_working_directory EXIT
module_records="$working_directory/go-module-license-records.jsonl"
subject_records="$working_directory/subject-records.jsonl"
: >"$module_records"
: >"$subject_records"
missing_go_license=false
for module_directory in "$repo_root" "$repo_root/runner"; do
  GOWORK=off go -C "$module_directory" list -m -json all |
    jq -r '
      select(.Main != true and .Version != null and .Dir != null) |
      [.Path, .Version, .Dir] | @tsv
    '
done | sort -u >"$working_directory/go-modules.tsv"

while IFS=$'\t' read -r module_path module_version module_directory; do
  module_identity_sha256="$(printf '%s@%s' "$module_path" "$module_version" | sha256sum | awk '{print $1}')"
  module_license_directory="$output_directory/go-license-texts/$module_identity_sha256"
  install -d -m 0755 "$module_license_directory"
  license_count=0
  while IFS= read -r license_path; do
    license_name="$(basename "$license_path")"
    install -m 0644 "$license_path" "$module_license_directory/$license_name"
    license_count="$((license_count + 1))"
  done < <(
    find "$module_directory" -maxdepth 2 -type f \
      \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) \
      -print | LC_ALL=C sort
  )
  module_status="collected"
  if [[ "$license_count" -eq 0 ]]; then
    module_status="missing-license-text"
    missing_go_license=true
  fi
  jq -cn \
    --arg path "$module_path" \
    --arg version "$module_version" \
    --arg directory "go-license-texts/$module_identity_sha256" \
    --arg status "$module_status" \
    --argjson licenseCount "$license_count" \
    '{
      path: $path, version: $version, licenseDirectory: $directory,
      licenseCount: $licenseCount, status: $status
    }' >>"$module_records"
done <"$working_directory/go-modules.tsv"
jq -s . "$module_records" >"$output_directory/go-license-inventory.json"

unknown_npm_licenses="$(jq '[.[] | select(.license == "UNKNOWN")] | length' "$output_directory/npm-license-inventory.json")"
source_license_status="passed"
if [[ "$missing_go_license" == true || "$unknown_npm_licenses" -ne 0 ]]; then
  source_license_status="blocked"
fi

evidence_artifact() {
  local artifact_path="$1"
  local canonical_path
  if [[ -L "$artifact_path" || ! -f "$artifact_path" ]]; then
    return 1
  fi
  canonical_path="$(realpath -e -- "$artifact_path")"
  if [[ "$canonical_path" != "$evidence_directory"/* ]]; then
    return 1
  fi
  jq -cn \
    --arg path "${canonical_path#"$evidence_directory"/}" \
    --arg sha256 "$(sha256sum "$canonical_path" | awk '{print $1}')" \
    '{path: $path, sha256: $sha256}'
}

subject_field() {
  local subject_id="$1"
  local expression="$2"
  jq -r --arg subject_id "$subject_id" "
    .subjects[] |
    select(.id == \$subject_id) |
    $expression
  " "$subject_manifest"
}

record_subject_license() {
  local subject_id="$1"
  local status="$2"
  local summary="$3"
  shift 3
  local artifacts='[]'
  if [[ "$#" -gt 0 ]]; then
    artifacts="$(
      for artifact_path in "$@"; do
        [[ -f "$artifact_path" && ! -L "$artifact_path" ]] || continue
        evidence_artifact "$artifact_path"
      done | jq -s .
    )"
  fi
  jq -cn \
    --arg subjectId "$subject_id" \
    --arg subjectSHA256 "$(subject_field "$subject_id" '.digest.sha256 // "0000000000000000000000000000000000000000000000000000000000000000"')" \
    --arg status "$status" \
    --arg summary "$summary" \
    --argjson artifacts "$artifacts" \
    '{
      subjectId: $subjectId,
      subjectSHA256: $subjectSHA256,
      status: $status,
      summary: $summary,
      artifacts: $artifacts
    }' >>"$subject_records"
}

common_license_artifacts=(
  "$output_directory/LICENSE"
  "$output_directory/THIRD_PARTY_NOTICES.md"
  "$output_directory/go-license-inventory.json"
  "$output_directory/npm-license-inventory.json"
  "$output_directory/FLUE_RUNTIME_BETA9_LICENSE.txt"
  "$output_directory/flue-runtime-beta9-source.json"
)
for subject_id in \
  linux-release-package \
  secondbox \
  secondbox-artifact-evidence \
  secondbox-guest-agent \
  secondbox-runner \
  secondbox-runner-identity \
  secondboxd \
  go-sdk-package; do
  if [[ "$(subject_field "$subject_id" '.status')" != "passed" ]]; then
    record_subject_license "$subject_id" blocked "The exact release subject is unavailable"
  elif [[ "$source_license_status" != "passed" ]]; then
    record_subject_license \
      "$subject_id" \
      blocked \
      "One or more pinned source dependency license texts is unavailable" \
      "${common_license_artifacts[@]}"
  else
    record_subject_license \
      "$subject_id" \
      passed \
      "Root notices and exact pinned Go dependency license texts cover this subject" \
      "${common_license_artifacts[@]}"
  fi
done

typescript_subject_status="$(subject_field typescript-sdk-package '.status')"
if [[ "$typescript_subject_status" != "passed" ]]; then
  record_subject_license typescript-sdk-package blocked "The exact TypeScript SDK package is unavailable"
else
  typescript_archive="$evidence_directory/$(subject_field typescript-sdk-package '.locator')"
  typescript_license_directory="$output_directory/typescript-sdk-license-files"
  install -d -m 0755 "$typescript_license_directory"
  typescript_license_status="passed"
  for package_path in \
    package/LICENSE \
    package/flue-runtime-beta9-LICENSE.txt \
    package/flue-runtime-beta9-source.json; do
    if ! tar -xOf "$typescript_archive" "$package_path" \
      >"$typescript_license_directory/$(basename "$package_path")"; then
      typescript_license_status="failed"
    fi
  done
  mapfile -t typescript_license_artifacts < <(
    find "$typescript_license_directory" -type f -print | LC_ALL=C sort
  )
  if [[ "$typescript_license_status" == "passed" && "$source_license_status" == "passed" ]]; then
    record_subject_license \
      typescript-sdk-package \
      passed \
      "The exact npm package contains the SecondBox and frozen Flue license/source notices" \
      "${typescript_license_artifacts[@]}" \
      "$output_directory/npm-license-inventory.json"
  else
    record_subject_license \
      typescript-sdk-package \
      failed \
      "The exact npm package is missing a required license or source notice" \
      "${typescript_license_artifacts[@]}"
  fi
fi

guest_subject_status="$(subject_field guest-execution-bundle '.status')"
if [[ "$guest_subject_status" != "passed" ]]; then
  record_subject_license guest-execution-bundle blocked "The exact signed guest bundle is unavailable"
else
  guest_archive="$evidence_directory/$(subject_field guest-execution-bundle '.locator')"
  guest_extract="$working_directory/guest"
  install -d -m 0755 "$guest_extract"
  tar --extract --gzip --file "$guest_archive" --directory "$guest_extract" --no-same-owner
  guest_root="$(find "$guest_extract" -mindepth 1 -maxdepth 1 -type d -print -quit)"
  guest_license_directory="$output_directory/guest-license-inventory"
  install -d -m 0755 "$guest_license_directory"
  guest_license_status="passed"
  for guest_license_file in \
    rootfs-debian-license-inventory.json \
    rootfs-python-license-inventory.json \
    rootfs-source-manifest.json \
    kernel-provenance.json; do
    if [[ ! -f "$guest_root/$guest_license_file" ]]; then
      guest_license_status="failed"
      continue
    fi
    install -m 0644 "$guest_root/$guest_license_file" "$guest_license_directory/$guest_license_file"
  done
  if [[ "$guest_license_status" == "passed" ]] &&
     ! jq -e '
       (.packages | length > 0) and
       all(.packages[]; .copyrightSha256 | test("^[0-9a-f]{64}$"))
     ' "$guest_license_directory/rootfs-debian-license-inventory.json" >/dev/null; then
    guest_license_status="failed"
  fi
  if [[ "$guest_license_status" == "passed" ]] &&
     ! jq -e '
       all(
         .distributions[];
         .license != "" or
         .licenseExpression != "" or
         (.licenseClassifiers | length > 0)
       )
     ' "$guest_license_directory/rootfs-python-license-inventory.json" >/dev/null; then
    guest_license_status="blocked"
  fi
  if [[ "$guest_license_status" == "passed" ]] && command -v debugfs >/dev/null 2>&1; then
    if ! debugfs \
      -R 'cat /usr/share/common-licenses/GPL-2' \
      "$guest_root/rootfs.ext4" \
      >"$guest_license_directory/linux-GPL-2.txt" 2>/dev/null; then
      guest_license_status="failed"
    fi
  else
    guest_license_status="blocked"
  fi
  mapfile -t guest_license_artifacts < <(
    find "$guest_license_directory" -type f -print | LC_ALL=C sort
  )
  if [[ "$guest_license_status" == "passed" ]]; then
    record_subject_license \
      guest-execution-bundle \
      passed \
      "The signed guest bundle binds Debian/Python license inventories and the Linux GPL-2 text from the exact rootfs" \
      "${guest_license_artifacts[@]}"
  else
    record_subject_license \
      guest-execution-bundle \
      "$guest_license_status" \
      "The signed guest bundle lacks complete package or kernel license evidence" \
      "${guest_license_artifacts[@]}"
  fi
fi

guest_artifact_image_status="$(subject_field guest-artifact-image '.status')"
if [[ "$guest_artifact_image_status" != "passed" ]]; then
  record_subject_license guest-artifact-image blocked "The exact guest artifact transport image is unavailable"
elif [[ "$guest_subject_status" != "passed" ]]; then
  record_subject_license \
    guest-artifact-image \
    failed \
    "The guest artifact image claims availability without its required signed guest bundle"
elif [[ "$guest_license_status" == "passed" ]]; then
  record_subject_license \
    guest-artifact-image \
    passed \
    "The image binding covers the same signed guest bundle license inventories and Linux GPL-2 text" \
    "${guest_license_artifacts[@]}"
else
  record_subject_license \
    guest-artifact-image \
    "$guest_license_status" \
    "The signed guest bundle bound into the transport image lacks complete package or kernel license evidence" \
    "${guest_license_artifacts[@]}"
fi

collect_image_license_evidence() {
  local subject_id="$1"
  local image_reference="$2"
  local image_license_directory="$output_directory/$subject_id-license-files"
  local container_id
  local copied_files

  if ! command -v docker >/dev/null 2>&1; then
    return 2
  fi
  if ! docker image inspect "$image_reference" >/dev/null 2>&1; then
    return 2
  fi
  container_id="$(docker create "$image_reference")"
  if ! install -d -m 0755 "$image_license_directory" ||
     ! docker cp "$container_id:/usr/share/licenses/." "$image_license_directory/licenses" ||
     ! docker cp "$container_id:/usr/share/doc/." "$image_license_directory/doc"; then
    docker rm "$container_id" >/dev/null
    return 1
  fi
  docker rm "$container_id" >/dev/null
  find "$image_license_directory" -type f \
    ! \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'copyright' \) \
    -delete
  copied_files="$(find "$image_license_directory" -type f | wc -l)"
  if [[ "$copied_files" -eq 0 ]]; then
    return 1
  fi
  while IFS= read -r license_path; do
    jq -cn \
      --arg path "${license_path#"$output_directory"/}" \
      --arg sha256 "$(sha256sum "$license_path" | awk '{print $1}')" \
      '{path: $path, sha256: $sha256}'
  done < <(find "$image_license_directory" -type f -print | LC_ALL=C sort) |
    jq -s . >"$output_directory/$subject_id-license-inventory.json"
}

for subject_id in control-plane-image runner-image; do
  if [[ "$(subject_field "$subject_id" '.status')" != "passed" ]]; then
    record_subject_license "$subject_id" blocked "The exact registry-backed image is unavailable"
    continue
  fi
  image_reference="$(subject_field "$subject_id" '.locator')"
  set +e
  collect_image_license_evidence "$subject_id" "$image_reference"
  image_license_exit="$?"
  set -e
  image_inventory="$output_directory/$subject_id-license-inventory.json"
  if [[ "$image_license_exit" -eq 0 ]]; then
    record_subject_license \
      "$subject_id" \
      passed \
      "License, copying, notice, and Debian copyright texts were extracted from the exact image digest without executing it" \
      "$image_inventory"
  elif [[ "$image_license_exit" -eq 2 ]]; then
    record_subject_license \
      "$subject_id" \
      blocked \
      "The exact image digest is not available in a local Docker image store for license extraction"
  else
    record_subject_license \
      "$subject_id" \
      failed \
      "License extraction from the exact image digest failed"
  fi
done

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
      evidenceType: "license",
      releaseVersion: $releaseVersion,
      sourceCommit: $sourceCommit,
      status: $status,
      summary: (
        if $status == "passed" then
          "Every exact release image, binary, guest asset bundle, and SDK package has bound license and notice evidence"
        elif $status == "failed" then
          "One or more exact release subjects has invalid or incomplete license evidence"
        else
          "One or more exact release subjects lacks license or notice evidence"
        end
      ),
      subjects: .
    }
  ' "$subject_records" >"$output_directory/license-status.json"

node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
  "$subject_manifest" \
  "$output_directory/license-status.json" \
  license
cleanup_license_evidence_working_directory
trap - EXIT

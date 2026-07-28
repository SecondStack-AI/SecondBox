#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 2 ]]; then
  echo "Usage: scripts/generate-dependency-age-evidence.sh OUTPUT.json RELEASE_SUBJECTS.json" >&2
  exit 2
fi

output_path="$1"
subject_manifest="$(realpath -e -- "$2")"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence_directory="$(dirname "$subject_manifest")"
inventory_path="$(dirname "$output_path")/dependency-age-inventory.json"
release_version="${SECONDBOX_RELEASE_VERSION:?set SECONDBOX_RELEASE_VERSION}"
source_commit="${SECONDBOX_RELEASE_SOURCE_COMMIT:?set SECONDBOX_RELEASE_SOURCE_COMMIT}"
if [[ -e "$output_path" ]]; then
  echo "Refusing to overwrite dependency-age evidence: $output_path" >&2
  exit 1
fi
if [[ -e "$inventory_path" ]]; then
  echo "Refusing to overwrite dependency-age inventory: $inventory_path" >&2
  exit 1
fi
if [[ -L "$2" || ! -f "$subject_manifest" ||
      "$(dirname "$(realpath -m -- "$output_path")")" != "$evidence_directory" ]]; then
  echo "SecondBox dependency-age output and subject manifest must share one evidence directory" >&2
  exit 1
fi
for required_command in curl date go jq node realpath sha256sum tar; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox dependency-age evidence requires command: $required_command" >&2
    exit 1
  fi
done

minimum_age_seconds="${SECONDBOX_DEPENDENCY_MINIMUM_AGE_SECONDS:?set SECONDBOX_DEPENDENCY_MINIMUM_AGE_SECONDS}"
evidence_timestamp="${SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP:?set SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP}"
if [[ ! "$minimum_age_seconds" =~ ^[0-9]+$ ]] || (( minimum_age_seconds < 1 )); then
  echo "SECONDBOX_DEPENDENCY_MINIMUM_AGE_SECONDS must be a positive integer" >&2
  exit 1
fi
evidence_epoch="$(date --date "$evidence_timestamp" +%s)"
working_directory="$(mktemp -d)"
cleanup_dependency_age_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox dependency-age evidence failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_dependency_age_working_directory EXIT
records_path="$working_directory/records.jsonl"
: >"$records_path"

root_go_subjects='["linux-release-package","secondbox","secondbox-runner-identity","secondboxd","control-plane-image","go-sdk-package"]'
runner_go_subjects='["linux-release-package","secondbox-artifact-evidence","secondbox-guest-agent","secondbox-runner","runner-image"]'
typescript_subjects='["typescript-sdk-package"]'
control_plane_image_subjects='["control-plane-image"]'
runner_image_subjects='["runner-image"]'
deployment_subjects='["linux-release-package"]'
guest_subjects='["guest-execution-bundle","guest-artifact-image"]'

evaluate_dependency_age() {
  local published_at="$1"
  dependency_age_seconds=-1
  dependency_age_status="missing-publication-time"
  if [[ -z "$published_at" ]] || ! published_epoch="$(date --date "$published_at" +%s 2>/dev/null)"; then
    return
  fi
  dependency_age_seconds="$((evidence_epoch - published_epoch))"
  dependency_age_status="eligible"
  if (( dependency_age_seconds < minimum_age_seconds )); then
    dependency_age_status="too-new"
  fi
}

append_dependency_record() {
  local ecosystem="$1"
  local name="$2"
  local version="$3"
  local registry="$4"
  local published_at="$5"
  local status="$6"
  local age_seconds="$7"
  local subjects="$8"
  local details="{}"
  if (( $# >= 9 )); then
    details="$9"
  fi
  jq -cn \
    --arg ecosystem "$ecosystem" \
    --arg name "$name" \
    --arg version "$version" \
    --arg registry "$registry" \
    --arg publishedAt "$published_at" \
    --arg status "$status" \
    --argjson ageSeconds "$age_seconds" \
    --argjson subjects "$subjects" \
    --argjson details "$details" \
    '{
      ecosystem: $ecosystem,
      name: $name,
      version: $version,
      registry: $registry,
      publishedAt: $publishedAt,
      ageSeconds: $ageSeconds,
      status: $status,
      subjects: $subjects
    } + $details' >>"$records_path"
}

last_response_header() {
  local headers_path="$1"
  local header_name="$2"
  awk -v requested_name="$header_name" '
    BEGIN {
      IGNORECASE = 1
    }
    {
      line = $0
      sub(/\r$/, "", line)
      separator = index(line, ":")
      if (separator > 0 && tolower(substr(line, 1, separator - 1)) == tolower(requested_name)) {
        value = substr(line, separator + 1)
        sub(/^[[:space:]]+/, "", value)
      }
    }
    END {
      print value
    }
  ' "$headers_path"
}

collect_go_modules() {
  local module_directory="$1"
  GOWORK=off go -C "$module_directory" list -m -json all |
    jq -r '
      select(.Main != true and .Version != null) |
      [.Path, .Version] | @tsv
    '
}

collect_go_modules "$repo_root" |
  awk -v subjects="$root_go_subjects" -F '\t' '
    {
      print $1 "\t" $2 "\t" subjects
    }
  ' >"$working_directory/go-modules.tsv"
collect_go_modules "$repo_root/runner" |
  awk -v subjects="$runner_go_subjects" -F '\t' '
    {
      print $1 "\t" $2 "\t" subjects
    }
  ' >>"$working_directory/go-modules.tsv"
LC_ALL=C sort -u -o "$working_directory/go-modules.tsv" "$working_directory/go-modules.tsv"

while IFS=$'\t' read -r module_path module_version subjects; do
  escaped_module_path="$(jq -nr --arg value "$module_path" '
    $value |
    explode |
    map(if . >= 65 and . <= 90 then [33, . + 32] else [.] end) |
    add |
    implode
  ')"
  escaped_module_version="$(jq -nr --arg value "$module_version" '
    $value |
    explode |
    map(if . >= 65 and . <= 90 then [33, . + 32] else [.] end) |
    add |
    implode
  ')"
  registry_url="https://proxy.golang.org/$escaped_module_path/@v/$escaped_module_version.info"
  if ! registry_response="$(curl --fail --silent --show-error "$registry_url")"; then
    append_dependency_record \
      "go" "$module_path" "$module_version" "$registry_url" "" \
      "registry-unavailable" -1 "$subjects"
    continue
  fi
  published_at="$(jq -r '.Time // empty' <<<"$registry_response")"
  evaluate_dependency_age "$published_at"
  append_dependency_record \
    "go" "$module_path" "$module_version" "$registry_url" "$published_at" \
    "$dependency_age_status" "$dependency_age_seconds" "$subjects"
done <"$working_directory/go-modules.tsv"

jq -r '
  .packages |
  to_entries[] |
  select(.key | test("(^|/)node_modules/")) |
  select(.value.version != null) |
  [
    (
      .value.name //
      (
        .key |
        capture(".*node_modules/(?<package>(?:@[^/]+/)?[^/]+)$").package
      )
    ),
    .value.version
  ] | @tsv
' "$repo_root/package-lock.json" | sort -u >"$working_directory/npm-packages.tsv"

while IFS=$'\t' read -r package_name package_version; do
  encoded_name="$(jq -nr --arg value "$package_name" '$value | @uri')"
  registry_url="https://registry.npmjs.org/$encoded_name"
  if ! registry_response="$(curl --fail --silent --show-error "$registry_url")"; then
    append_dependency_record \
      "npm" "$package_name" "$package_version" "$registry_url" "" \
      "registry-unavailable" -1 "$typescript_subjects"
    continue
  fi
  published_at="$(jq -r --arg version "$package_version" '.time[$version] // empty' <<<"$registry_response")"
  evaluate_dependency_age "$published_at"
  append_dependency_record \
    "npm" "$package_name" "$package_version" "$registry_url" "$published_at" \
    "$dependency_age_status" "$dependency_age_seconds" "$typescript_subjects"
done <"$working_directory/npm-packages.tsv"

sed -n 's|^FROM[[:space:]]\+\(docker\.io/[^[:space:]]*\).*|\1|p' "$repo_root/Dockerfile" |
  awk -v subjects="$control_plane_image_subjects" '{print $0 "\t" subjects}' \
    >"$working_directory/oci-images.tsv"
sed -n 's|^FROM[[:space:]]\+\(docker\.io/[^[:space:]]*\).*|\1|p' "$repo_root/runner/Dockerfile" |
  awk -v subjects="$runner_image_subjects" '{print $0 "\t" subjects}' \
    >>"$working_directory/oci-images.tsv"
sed -n \
  -e 's|^SECONDBOX_POSTGRES_IMAGE=\(docker\.io/.*\)$|\1|p' \
  -e 's|^SECONDBOX_OBJECT_STORE_IMAGE=\(docker\.io/.*\)$|\1|p' \
  "$repo_root/deploy/environment.example" |
  awk -v subjects="$deployment_subjects" '{print $0 "\t" subjects}' \
    >>"$working_directory/oci-images.tsv"
LC_ALL=C sort -u -o "$working_directory/oci-images.tsv" "$working_directory/oci-images.tsv"

while IFS=$'\t' read -r image_reference subjects; do
  image_without_registry="${image_reference#docker.io/}"
  repository_path="${image_without_registry%:*}"
  image_tag="${image_without_registry##*:}"
  if [[ "$repository_path" == "$image_without_registry" || -z "$image_tag" ]]; then
    echo "SecondBox dependency-age evidence requires a tagged Docker Hub image: $image_reference" >&2
    exit 1
  fi
  registry_url="https://hub.docker.com/v2/repositories/$repository_path/tags/$image_tag"
  if ! registry_response="$(curl --fail --silent --show-error "$registry_url")"; then
    append_dependency_record \
      "oci" "docker.io/$repository_path" "$image_tag" "$registry_url" "" \
      "registry-unavailable" -1 "$subjects"
    continue
  fi
  published_at="$(jq -r '.last_updated // empty' <<<"$registry_response")"
  image_digest="$(jq -r '.digest // empty' <<<"$registry_response")"
  if [[ -z "$published_at" || ! "$image_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    dependency_age_seconds=-1
    dependency_age_status="missing-publication-time-or-digest"
  else
    evaluate_dependency_age "$published_at"
  fi
  append_dependency_record \
    "oci" "docker.io/$repository_path" "$image_tag" "$registry_url" "$published_at" \
    "$dependency_age_status" "$dependency_age_seconds" "$subjects" \
    "$(jq -cn --arg digest "$image_digest" '{digest: $digest}')"
done <"$working_directory/oci-images.tsv"

firecracker_lock="$repo_root/runner/internal/firecracker/firecracker.lock"
firecracker_version="$(sed -n 's/^FIRECRACKER_VERSION=//p' "$firecracker_lock")"
dockerfile_firecracker_version="$(
  sed -n 's/^ARG FIRECRACKER_VERSION=//p' "$repo_root/runner/Dockerfile"
)"
firecracker_archive_sha256="$(
  sed -n 's/^ARG FIRECRACKER_ARCHIVE_SHA256=//p' "$repo_root/runner/Dockerfile"
)"
firecracker_registry_url="$(
  printf 'https://github.com/firecracker-microvm/firecracker/releases/download/v%s/firecracker-v%s-x86_64.tgz' \
    "$firecracker_version" \
    "$firecracker_version"
)"
if [[ ! "$firecracker_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ||
      "$dockerfile_firecracker_version" != "$firecracker_version" ||
      ! "$firecracker_archive_sha256" =~ ^[0-9a-f]{64}$ ]]; then
  append_dependency_record \
    "github-release-asset" "firecracker-microvm/firecracker" "$firecracker_version" \
    "$firecracker_registry_url" "" "invalid-pinned-input" -1 "$runner_image_subjects" \
    "$(jq -cn \
      --arg dockerfileVersion "$dockerfile_firecracker_version" \
      --arg sha256 "$firecracker_archive_sha256" \
      '{dockerfileVersion: $dockerfileVersion, sha256: $sha256}')"
else
  firecracker_headers="$working_directory/firecracker.headers"
  if curl \
    --fail \
    --silent \
    --show-error \
    --location \
    --head \
    --dump-header "$firecracker_headers" \
    --output /dev/null \
    "$firecracker_registry_url"; then
    firecracker_published_at="$(last_response_header "$firecracker_headers" "last-modified")"
    evaluate_dependency_age "$firecracker_published_at"
    append_dependency_record \
      "github-release-asset" "firecracker-microvm/firecracker" "$firecracker_version" \
      "$firecracker_registry_url" "$firecracker_published_at" \
      "$dependency_age_status" "$dependency_age_seconds" "$runner_image_subjects" \
      "$(jq -cn --arg sha256 "$firecracker_archive_sha256" '{sha256: $sha256}')"
  else
    append_dependency_record \
      "github-release-asset" "firecracker-microvm/firecracker" "$firecracker_version" \
      "$firecracker_registry_url" "" "registry-unavailable" -1 "$runner_image_subjects" \
      "$(jq -cn --arg sha256 "$firecracker_archive_sha256" '{sha256: $sha256}')"
  fi
fi

kernel_lock="$repo_root/runner/scripts/microvm-image/kernel.lock"
kernel_version="$(sed -n 's/^KERNEL_VERSION=//p' "$kernel_lock")"
kernel_registry_url="$(sed -n 's/^KERNEL_URL=//p' "$kernel_lock")"
kernel_sha256="$(sed -n 's/^KERNEL_SHA256=//p' "$kernel_lock")"
kernel_source_date_epoch="$(sed -n 's/^KERNEL_SOURCE_DATE_EPOCH=//p' "$kernel_lock")"
expected_kernel_url="https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$kernel_version.tar.xz"
if [[ ! "$kernel_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ||
      "$kernel_registry_url" != "$expected_kernel_url" ||
      ! "$kernel_sha256" =~ ^[0-9a-f]{64}$ ||
      ! "$kernel_source_date_epoch" =~ ^[0-9]+$ ]]; then
  append_dependency_record \
    "kernel.org-release-asset" "linux" "$kernel_version" "$kernel_registry_url" "" \
    "invalid-pinned-input" -1 "$guest_subjects" \
    "$(jq -cn \
      --arg sha256 "$kernel_sha256" \
      --arg sourceDateEpoch "$kernel_source_date_epoch" \
      '{sha256: $sha256, sourceDateEpoch: $sourceDateEpoch}')"
else
  kernel_headers="$working_directory/kernel.headers"
  if curl \
    --fail \
    --silent \
    --show-error \
    --head \
    --dump-header "$kernel_headers" \
    --output /dev/null \
    "$kernel_registry_url"; then
    kernel_published_at="$(last_response_header "$kernel_headers" "last-modified")"
    evaluate_dependency_age "$kernel_published_at"
    append_dependency_record \
      "kernel.org-release-asset" "linux" "$kernel_version" "$kernel_registry_url" \
      "$kernel_published_at" "$dependency_age_status" "$dependency_age_seconds" \
      "$guest_subjects" \
      "$(jq -cn \
        --arg sha256 "$kernel_sha256" \
        --argjson sourceDateEpoch "$kernel_source_date_epoch" \
        '{sha256: $sha256, sourceDateEpoch: $sourceDateEpoch}')"
  else
    append_dependency_record \
      "kernel.org-release-asset" "linux" "$kernel_version" "$kernel_registry_url" "" \
      "registry-unavailable" -1 "$guest_subjects" \
      "$(jq -cn \
        --arg sha256 "$kernel_sha256" \
        --argjson sourceDateEpoch "$kernel_source_date_epoch" \
        '{sha256: $sha256, sourceDateEpoch: $sourceDateEpoch}')"
  fi
fi

debian_definition="$repo_root/runner/scripts/microvm-image/rootfs/secondbox-debian-image-definition.json"
debian_suite="$(jq -r '.debian.suite // empty' "$debian_definition")"
debian_architecture="$(jq -r '.debian.architecture // empty' "$debian_definition")"
debian_snapshot_url="$(jq -r '.debian.snapshot // empty' "$debian_definition")"
debian_snapshot_timestamp="$(
  sed -n 's|^https://snapshot\.debian\.org/archive/debian/\([0-9]\{8\}T[0-9]\{6\}Z\)/$|\1|p' \
    <<<"$debian_snapshot_url"
)"
if [[ -z "$debian_snapshot_timestamp" ||
      "$debian_suite" != "bookworm" ||
      "$debian_architecture" != "amd64" ]]; then
  append_dependency_record \
    "debian-snapshot" "debian" "$debian_suite/$debian_snapshot_timestamp" \
    "$debian_snapshot_url" "" "invalid-pinned-input" -1 "$guest_subjects" \
    "$(jq -cn --arg architecture "$debian_architecture" '{architecture: $architecture}')"
else
  debian_published_at="$(
    printf '%s-%s-%sT%s:%s:%sZ' \
      "${debian_snapshot_timestamp:0:4}" \
      "${debian_snapshot_timestamp:4:2}" \
      "${debian_snapshot_timestamp:6:2}" \
      "${debian_snapshot_timestamp:9:2}" \
      "${debian_snapshot_timestamp:11:2}" \
      "${debian_snapshot_timestamp:13:2}"
  )"
  if debian_effective_url="$(
    curl \
      --fail \
      --silent \
      --show-error \
      --location \
      --head \
      --output /dev/null \
      --write-out '%{url_effective}' \
      "$debian_snapshot_url"
  )" &&
    [[ "$debian_effective_url" =~ ^https://snapshot\.debian\.org/archive/debian/[0-9]{8}T[0-9]{6}Z/$ ]]; then
    evaluate_dependency_age "$debian_published_at"
    append_dependency_record \
      "debian-snapshot" "debian" "$debian_suite/$debian_snapshot_timestamp" \
      "$debian_snapshot_url" "$debian_published_at" \
      "$dependency_age_status" "$dependency_age_seconds" "$guest_subjects" \
      "$(jq -cn \
        --arg architecture "$debian_architecture" \
        --arg resolvedSnapshot "$debian_effective_url" \
        '{architecture: $architecture, resolvedSnapshot: $resolvedSnapshot}')"
  else
    append_dependency_record \
      "debian-snapshot" "debian" "$debian_suite/$debian_snapshot_timestamp" \
      "$debian_snapshot_url" "" "registry-unavailable" -1 "$guest_subjects" \
      "$(jq -cn --arg architecture "$debian_architecture" '{architecture: $architecture}')"
  fi
fi

guest_subject_status="$(
  jq -r '.subjects[] | select(.id == "guest-execution-bundle") | .status' "$subject_manifest"
)"
if [[ "$guest_subject_status" == "passed" ]]; then
  guest_subject_locator="$(
    jq -r '.subjects[] | select(.id == "guest-execution-bundle") | .locator // empty' \
      "$subject_manifest"
  )"
  guest_archive_candidate="$evidence_directory/$guest_subject_locator"
  if [[ -L "$guest_archive_candidate" ||
        ! -f "$guest_archive_candidate" ||
        "$(realpath -e -- "$guest_archive_candidate")" != "$guest_archive_candidate" ||
        "$guest_archive_candidate" != "$evidence_directory/"* ]]; then
    append_dependency_record \
      "pypi" "guest-python-freeze" "unresolved" "$guest_subject_locator" "" \
      "invalid-guest-archive" -1 "$guest_subjects"
  else
    guest_archive_members="$working_directory/guest-archive-members.txt"
    if ! tar -tzf "$guest_archive_candidate" >"$guest_archive_members"; then
      append_dependency_record \
        "pypi" "guest-python-freeze" "unresolved" "$guest_subject_locator" "" \
        "invalid-guest-archive" -1 "$guest_subjects"
    else
      unsafe_guest_member=""
      while IFS= read -r guest_member; do
        if [[ "$guest_member" == /* ||
              "$guest_member" == ".." ||
              "$guest_member" == ../* ||
              "$guest_member" == */../* ||
              "$guest_member" == *"/.." ||
              "$guest_member" == *\\* ]]; then
          unsafe_guest_member="$guest_member"
          break
        fi
      done <"$guest_archive_members"
      mapfile -t python_freeze_members < <(
        sed -n '\|^[^/][^/]*/rootfs-python\.freeze$|p' "$guest_archive_members"
      )
      if [[ -n "$unsafe_guest_member" || "${#python_freeze_members[@]}" -ne 1 ]]; then
        append_dependency_record \
          "pypi" "guest-python-freeze" "unresolved" "$guest_subject_locator" "" \
          "invalid-guest-archive" -1 "$guest_subjects" \
          "$(jq -cn \
            --arg unsafeMember "$unsafe_guest_member" \
            --argjson freezeMemberCount "${#python_freeze_members[@]}" \
            '{unsafeMember: $unsafeMember, freezeMemberCount: $freezeMemberCount}')"
      else
        python_freeze_path="$working_directory/rootfs-python.freeze"
        tar -xOzf "$guest_archive_candidate" "${python_freeze_members[0]}" >"$python_freeze_path"
        python_freeze_record_count=0
        while IFS= read -r freeze_line || [[ -n "$freeze_line" ]]; do
          freeze_line="${freeze_line%$'\r'}"
          if [[ -z "$freeze_line" || "$freeze_line" == \#* ]]; then
            continue
          fi
          python_freeze_record_count="$((python_freeze_record_count + 1))"
          if [[ ! "$freeze_line" =~ ^([A-Za-z0-9][A-Za-z0-9._-]*)==([^[:space:]]+)$ ]]; then
            append_dependency_record \
              "pypi" "guest-python-freeze" "$freeze_line" \
              "https://pypi.org/" "" "unsupported-freeze-entry" -1 "$guest_subjects"
            continue
          fi
          python_package_name="${BASH_REMATCH[1]}"
          python_package_version="${BASH_REMATCH[2]}"
          encoded_python_package_name="$(
            jq -nr --arg value "$python_package_name" '$value | @uri'
          )"
          encoded_python_package_version="$(
            jq -nr --arg value "$python_package_version" '$value | @uri'
          )"
          pypi_registry_url="https://pypi.org/pypi/$encoded_python_package_name/$encoded_python_package_version/json"
          if ! pypi_response="$(curl --fail --silent --show-error "$pypi_registry_url")"; then
            append_dependency_record \
              "pypi" "$python_package_name" "$python_package_version" \
              "$pypi_registry_url" "" "registry-unavailable" -1 "$guest_subjects"
            continue
          fi
          pypi_published_at="$(
            jq -r '
              if (.urls | type) == "array" and (.urls | length) > 0 then
                [.urls[].upload_time_iso_8601] | max // empty
              else
                empty
              end
            ' <<<"$pypi_response"
          )"
          pypi_file_count="$(jq -r '.urls | length' <<<"$pypi_response")"
          pypi_file_digests="$(
            jq -c '[.urls[].digests.sha256] | unique | sort' <<<"$pypi_response"
          )"
          if [[ "$(jq -r '.info.version // empty' <<<"$pypi_response")" != "$python_package_version" ||
                "$pypi_file_count" -lt 1 ||
                "$pypi_file_digests" == "[]" ]]; then
            dependency_age_seconds=-1
            dependency_age_status="invalid-registry-response"
          else
            evaluate_dependency_age "$pypi_published_at"
          fi
          append_dependency_record \
            "pypi" "$python_package_name" "$python_package_version" \
            "$pypi_registry_url" "$pypi_published_at" \
            "$dependency_age_status" "$dependency_age_seconds" "$guest_subjects" \
            "$(jq -cn \
              --argjson fileCount "$pypi_file_count" \
              --argjson fileSHA256s "$pypi_file_digests" \
              '{fileCount: $fileCount, fileSHA256s: $fileSHA256s}')"
        done <"$python_freeze_path"
        if (( python_freeze_record_count == 0 )); then
          append_dependency_record \
            "pypi" "guest-python-freeze" "empty" "$guest_subject_locator" "" \
            "empty-freeze" -1 "$guest_subjects"
        fi
      fi
    fi
  fi
fi

overall_status="$(
  jq -s -r '
    if all(.[]; .status == "eligible") then "passed" else "failed" end
  ' "$records_path"
)"
jq -s \
  --arg status "$overall_status" \
  --arg generatedAt "$evidence_timestamp" \
  --argjson minimumAgeSeconds "$minimum_age_seconds" '
    {
      schemaVersion: 1,
      status: $status,
      generatedAt: $generatedAt,
      minimumAgeSeconds: $minimumAgeSeconds,
      registries: [
        "https://proxy.golang.org",
        "https://registry.npmjs.org",
        "https://hub.docker.com/v2/repositories",
        "https://github.com/firecracker-microvm/firecracker/releases",
        "https://cdn.kernel.org/pub/linux/kernel",
        "https://snapshot.debian.org/archive/debian",
        "https://pypi.org/pypi"
      ],
      dependencies: .
    }
  ' "$records_path" >"$inventory_path"

inventory_sha256="$(sha256sum "$inventory_path" | awk '{print $1}')"
subject_records="$working_directory/subject-records.jsonl"
: >"$subject_records"
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
  guest-execution-bundle \
  guest-artifact-image \
  go-sdk-package \
  typescript-sdk-package; do
  subject_status="$(jq -r --arg subject_id "$subject_id" '.subjects[] | select(.id == $subject_id) | .status' "$subject_manifest")"
  subject_sha256="$(jq -r --arg subject_id "$subject_id" '.subjects[] | select(.id == $subject_id) | .digest.sha256 // "0000000000000000000000000000000000000000000000000000000000000000"' "$subject_manifest")"
  dependency_count="$(
    jq -s --arg subject_id "$subject_id" '
      [.[] | select(.subjects | index($subject_id))] | length
    ' "$records_path"
  )"
  ineligible_count="$(
    jq -s --arg subject_id "$subject_id" '
      [.[] | select(.subjects | index($subject_id)) | select(.status != "eligible")] | length
    ' "$records_path"
  )"
  record_status="passed"
  record_summary="$dependency_count exact dependency inputs have eligible authoritative publication timestamps"
  if [[ "$subject_status" != "passed" ]]; then
    record_status="blocked"
    record_summary="The exact release subject is unavailable"
  elif (( dependency_count == 0 )); then
    record_status="failed"
    record_summary="No dependency-age inputs are mapped to the exact release subject"
  elif (( ineligible_count > 0 )); then
    record_status="failed"
    record_summary="$ineligible_count of $dependency_count exact dependency inputs are too new, unavailable, or invalid"
  fi
  jq -cn \
    --arg subjectId "$subject_id" \
    --arg subjectSHA256 "$subject_sha256" \
    --arg status "$record_status" \
    --arg summary "$record_summary" \
    --arg path "$(basename "$inventory_path")" \
    --arg sha256 "$inventory_sha256" \
    '{
      subjectId: $subjectId,
      subjectSHA256: $subjectSHA256,
      status: $status,
      summary: $summary,
      artifacts: [{path: $path, sha256: $sha256}]
    }' >>"$subject_records"
done

report_status="passed"
if jq -e 'select(.status == "failed")' "$subject_records" >/dev/null; then
  report_status="failed"
elif [[ "$(jq -r '.status' "$subject_manifest")" != "passed" ]] ||
     jq -e 'select(.status != "passed")' "$subject_records" >/dev/null; then
  report_status="blocked"
fi
jq -s \
  --arg status "$report_status" \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" '
    {
      schemaVersion: 1,
      evidenceType: "dependency-age",
      releaseVersion: $releaseVersion,
      sourceCommit: $sourceCommit,
      status: $status,
      summary: (
        if $status == "passed" then
          "Every dependency of every exact release subject has registry-backed minimum-age evidence"
        elif $status == "failed" then
          "One or more release dependencies is too new or lacks registry evidence"
        else
          "One or more exact release subjects is unavailable"
        end
      ),
      subjects: .
    }
  ' "$subject_records" >"$output_path"
node "$repo_root/scripts/verify-release-supply-chain-coverage.mjs" \
  "$subject_manifest" \
  "$output_path" \
  dependency-age
cleanup_dependency_age_working_directory
trap - EXIT

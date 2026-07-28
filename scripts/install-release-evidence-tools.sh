#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 1 ]]; then
  echo "Usage: scripts/install-release-evidence-tools.sh OUTPUT_BIN_DIRECTORY" >&2
  exit 2
fi

output_directory="$1"
evidence_timestamp="${SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP:?set SECONDBOX_RELEASE_EVIDENCE_TIMESTAMP}"
minimum_age_seconds="${SECONDBOX_DEPENDENCY_MINIMUM_AGE_SECONDS:?set SECONDBOX_DEPENDENCY_MINIMUM_AGE_SECONDS}"
if [[ -L "$output_directory" || ! -d "$output_directory" ]]; then
  echo "SecondBox release evidence tool output must be an existing non-symbolic-link directory" >&2
  exit 1
fi
if [[ "$(uname -m)" != "x86_64" ]]; then
  echo "SecondBox release evidence tool pins currently qualify only Linux amd64" >&2
  exit 1
fi
if [[ ! "$minimum_age_seconds" =~ ^[0-9]+$ ]] || (( minimum_age_seconds < 1 )); then
  echo "SECONDBOX_DEPENDENCY_MINIMUM_AGE_SECONDS must be a positive integer" >&2
  exit 1
fi
for required_command in curl date grep install jq sha256sum tar uname; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox release evidence tool installation requires command: $required_command" >&2
    exit 1
  fi
done

working_directory="$(mktemp -d)"
cleanup_release_evidence_tool_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox release evidence tool installation failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_evidence_tool_working_directory EXIT
tool_manifest="$output_directory/release-evidence-tools.json"
for output_path in "$output_directory/syft" "$output_directory/grype" "$tool_manifest"; do
  if [[ -e "$output_path" ]]; then
    echo "SecondBox release evidence tool installation refuses to overwrite: $output_path" >&2
    exit 1
  fi
done

install_verified_release_tool() {
  local tool_name="$1"
  local version="$2"
  local published_at="$3"
  local archive_sha256="$4"
  local checksum_file_sha256="$5"
  local archive_name="${tool_name}_${version}_linux_amd64.tar.gz"
  local checksum_name="${tool_name}_${version}_checksums.txt"
  local release_base="https://github.com/anchore/$tool_name/releases/download/v$version"
  local archive_path="$working_directory/$archive_name"
  local checksum_path="$working_directory/$checksum_name"
  local published_epoch
  local evidence_epoch

  evidence_epoch="$(date --date "$evidence_timestamp" +%s)"
  published_epoch="$(date --date "$published_at" +%s)"
  if (( evidence_epoch - published_epoch < minimum_age_seconds )); then
    echo "SecondBox release evidence tool $tool_name $version is younger than the minimum dependency age" >&2
    return 1
  fi
  curl --fail --silent --show-error --location \
    "$release_base/$checksum_name" \
    --output "$checksum_path"
  printf '%s  %s\n' "$checksum_file_sha256" "$checksum_path" |
    sha256sum --check --strict
  if ! grep -Fxq "$archive_sha256  $archive_name" "$checksum_path"; then
    echo "SecondBox release evidence tool checksum list does not bind $archive_name" >&2
    return 1
  fi
  curl --fail --silent --show-error --location \
    "$release_base/$archive_name" \
    --output "$archive_path"
  printf '%s  %s\n' "$archive_sha256" "$archive_path" |
    sha256sum --check --strict
  tar --extract --gzip --file "$archive_path" --directory "$working_directory" "$tool_name"
  install -m 0755 "$working_directory/$tool_name" "$output_directory/$tool_name"
  "$output_directory/$tool_name" version >/dev/null
}

install_verified_release_tool \
  syft \
  1.44.0 \
  2026-05-01T17:17:27Z \
  0e91737aee2b5baf1d255b959630194a302335d848ff97bb07921eb6205b5f5a \
  fa24ce6cafe6edbdba166414ce79de8142fbc217f8167e418dfb09e5aedfbf4e
install_verified_release_tool \
  grype \
  0.112.0 \
  2026-05-01T19:12:30Z \
  acb14a030010fe9bdb9594b4ae108d9d14ef2f926d936aa0916dc62c89c058ea \
  6294bee2e41b4af0c6f603b049b836b8dab25e39dac12343c7b69dfa9e7f1399

jq -n \
  --arg evidenceTimestamp "$evidence_timestamp" \
  --argjson minimumAgeSeconds "$minimum_age_seconds" \
  --arg syftSHA256 "$(sha256sum "$output_directory/syft" | awk '{print $1}')" \
  --arg grypeSHA256 "$(sha256sum "$output_directory/grype" | awk '{print $1}')" '
    {
      schemaVersion: 1,
      evidenceTimestamp: $evidenceTimestamp,
      minimumAgeSeconds: $minimumAgeSeconds,
      tools: [
        {
          name: "syft",
          version: "1.44.0",
          source: "https://github.com/anchore/syft/releases/tag/v1.44.0",
          archive: "syft_1.44.0_linux_amd64.tar.gz",
          archiveSHA256: "0e91737aee2b5baf1d255b959630194a302335d848ff97bb07921eb6205b5f5a",
          checksumFileSHA256: "fa24ce6cafe6edbdba166414ce79de8142fbc217f8167e418dfb09e5aedfbf4e",
          publishedAt: "2026-05-01T17:17:27Z",
          installedBinarySHA256: $syftSHA256
        },
        {
          name: "grype",
          version: "0.112.0",
          source: "https://github.com/anchore/grype/releases/tag/v0.112.0",
          archive: "grype_0.112.0_linux_amd64.tar.gz",
          archiveSHA256: "acb14a030010fe9bdb9594b4ae108d9d14ef2f926d936aa0916dc62c89c058ea",
          checksumFileSHA256: "6294bee2e41b4af0c6f603b049b836b8dab25e39dac12343c7b69dfa9e7f1399",
          publishedAt: "2026-05-01T19:12:30Z",
          installedBinarySHA256: $grypeSHA256
        }
      ]
    }
  ' >"$tool_manifest"

cleanup_release_evidence_tool_working_directory
trap - EXIT

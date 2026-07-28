#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 4 ]]; then
  echo "Usage: scripts/package-release-artifacts.sh BINARY_DIRECTORY OUTPUT_DIRECTORY RELEASE_VERSION SOURCE_COMMIT" >&2
  exit 2
fi

binary_directory="$(realpath -e -- "$1")"
output_directory="$(realpath -e -- "$2")"
release_version="$3"
source_commit="$4"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -L "$1" || ! -d "$binary_directory" || -L "$2" || ! -d "$output_directory" ]]; then
  echo "SecondBox release packaging requires regular input and output directories" >&2
  exit 1
fi
if [[ ! "$release_version" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  echo "SecondBox release version contains unsupported path characters: $release_version" >&2
  exit 1
fi
if [[ ! "$source_commit" =~ ^[0-9a-f]{40}$ ]] ||
   [[ "$(git -C "$repo_root" rev-parse HEAD)" != "$source_commit" ]]; then
  echo "SecondBox release packaging source commit must equal the checked-out 40-character commit" >&2
  exit 1
fi
if [[ -n "$(git -C "$repo_root" status --porcelain=v1 --untracked-files=all)" ]]; then
  echo "SecondBox release packaging requires a clean source tree so every packaged byte belongs to the source commit" >&2
  exit 1
fi
for required_command in cmp find git jq readelf sha256sum tar; do
  if ! command -v "$required_command" >/dev/null 2>&1; then
    echo "SecondBox release packaging requires command: $required_command" >&2
    exit 1
  fi
done

verify_linux_amd64_binary() {
  local binary_path="$1"
  local elf_header
  elf_header="$(readelf -h "$binary_path")"
  if ! grep -Eq 'Class:[[:space:]]+ELF64' <<<"$elf_header" ||
     ! grep -Eq "Data:[[:space:]]+2's complement, little endian" <<<"$elf_header" ||
     ! grep -Eq 'Machine:[[:space:]]+Advanced Micro Devices X86-64' <<<"$elf_header"; then
    echo "SecondBox linux-amd64 release package requires an ELF64 little-endian x86-64 binary: $binary_path" >&2
    exit 1
  fi
}

package_name="secondbox-$release_version-linux-amd64"
package_archive="$output_directory/$package_name.tar.gz"
package_manifest="$output_directory/$package_name.manifest.json"
package_checksums="$output_directory/$package_name.SHA256SUMS"
for output_path in "$package_archive" "$package_manifest" "$package_checksums"; do
  if [[ -e "$output_path" ]]; then
    echo "SecondBox release packaging refuses to overwrite: $output_path" >&2
    exit 1
  fi
done

working_directory="$(mktemp -d)"
package_root="$working_directory/$package_name"
cleanup_release_package_working_directory() {
  if [[ -d "$working_directory" ]] && ! rm -rf -- "$working_directory"; then
    echo "SecondBox release packaging failed to remove temporary directory: $working_directory" >&2
    return 1
  fi
}
trap cleanup_release_package_working_directory EXIT

install -d -m 0755 \
  "$package_root/bin" \
  "$package_root/contracts" \
  "$package_root/deploy" \
  "$package_root/docs/operations" \
  "$package_root/migrations" \
  "$package_root/runner/deploy"

for binary_name in \
  secondbox \
  secondbox-artifact-evidence \
  secondbox-guest-agent \
  secondbox-runner \
  secondbox-runner-identity \
  secondboxd; do
  binary_path="$binary_directory/$binary_name"
  if [[ -L "$binary_path" || ! -f "$binary_path" || ! -x "$binary_path" ]]; then
    echo "SecondBox release packaging requires executable binary: $binary_path" >&2
    exit 1
  fi
  verify_linux_amd64_binary "$binary_path"
  install -m 0755 "$binary_path" "$package_root/bin/$binary_name"
  if ! cmp -s "$binary_path" "$package_root/bin/$binary_name"; then
    echo "SecondBox release packaging changed binary bytes while copying: $binary_name" >&2
    exit 1
  fi
done

install -m 0644 "$repo_root/LICENSE" "$package_root/LICENSE"
install -m 0644 "$repo_root/THIRD_PARTY_NOTICES.md" "$package_root/THIRD_PARTY_NOTICES.md"
cp -a "$repo_root/contracts/." "$package_root/contracts/"
cp -a "$repo_root/migrations/." "$package_root/migrations/"
install -m 0644 \
  "$repo_root/release/current-compatibility.json" \
  "$package_root/current-compatibility.json"

for deployment_path in \
  compose.yml \
  environment.example \
  bin/bootstrap-environment.sh \
  bin/bootstrap-runner-trust.sh \
  bin/collect-support-bundle.sh \
  bin/diagnose-runner-host.sh \
  bin/export-audit.sh \
  bin/validate-environment.sh; do
  source_path="$repo_root/deploy/$deployment_path"
  if [[ -f "$source_path" ]]; then
    install -d -m 0755 "$package_root/deploy/$(dirname "$deployment_path")"
    deployment_mode=0644
    if [[ "$deployment_path" == bin/*.sh ]]; then
      deployment_mode=0755
    fi
    install -m "$deployment_mode" "$source_path" "$package_root/deploy/$deployment_path"
    if ! cmp -s "$source_path" "$package_root/deploy/$deployment_path"; then
      echo "SecondBox release packaging changed deployment bytes while copying: $deployment_path" >&2
      exit 1
    fi
  fi
done
install -m 0644 \
  "$repo_root/runner/deploy/secondbox-runner.service" \
  "$repo_root/runner/deploy/secondbox-runner-devnet.service" \
  "$repo_root/runner/deploy/microvm-artifact-transport.Dockerfile" \
  "$package_root/runner/deploy/"
for runner_deployment_path in \
  secondbox-runner.service \
  secondbox-runner-devnet.service \
  microvm-artifact-transport.Dockerfile; do
  if ! cmp -s \
    "$repo_root/runner/deploy/$runner_deployment_path" \
    "$package_root/runner/deploy/$runner_deployment_path"; then
    echo "SecondBox release packaging changed Runner deployment bytes while copying: $runner_deployment_path" >&2
    exit 1
  fi
done
for operations_document in \
  compatibility.md \
  deployment.md \
  qualification.md \
  release-evidence.md; do
  source_path="$repo_root/docs/operations/$operations_document"
  if [[ -f "$source_path" ]]; then
    install -m 0644 "$source_path" "$package_root/docs/operations/$operations_document"
  fi
done

if find "$package_root" -type l -print -quit | grep -q .; then
  echo "SecondBox release package must not contain symbolic links" >&2
  exit 1
fi

package_file_records="$working_directory/package-files.jsonl"
while IFS= read -r relative_path; do
  file_path="$package_root/$relative_path"
  jq -cn \
    --arg path "$relative_path" \
    --arg sha256 "$(sha256sum "$file_path" | awk '{print $1}')" \
    --arg mode "$(stat -c %a "$file_path")" \
    --argjson size "$(stat -c %s "$file_path")" \
    '{path: $path, sha256: $sha256, size: $size, mode: $mode}' >>"$package_file_records"
done < <(
  cd "$package_root"
  find . -type f -printf '%P\n' | LC_ALL=C sort
)
jq -s \
  --arg releaseVersion "$release_version" \
  --arg sourceCommit "$source_commit" \
  '{
    schemaVersion: 1,
    releaseVersion: $releaseVersion,
    sourceCommit: $sourceCommit,
    files: .
  }' "$package_file_records" >"$package_root/release-package-manifest.json"

source_epoch="$(git -C "$repo_root" show -s --format=%ct "$source_commit")"
tar \
  --sort=name \
  --mtime="@$source_epoch" \
  --owner=0 \
  --group=0 \
  --numeric-owner \
  -C "$working_directory" \
  -czf "$package_archive" \
  "$package_name"
jq \
  --arg packagePath "$(basename "$package_archive")" \
  --arg packageSHA256 "$(sha256sum "$package_archive" | awk '{print $1}')" \
  --argjson packageSize "$(stat -c %s "$package_archive")" \
  '. + {
    packageArchive: {
      path: $packagePath,
      sha256: $packageSHA256,
      size: $packageSize
    }
  }' "$package_root/release-package-manifest.json" >"$package_manifest"
(
  cd "$output_directory"
  sha256sum "$(basename "$package_archive")" "$(basename "$package_manifest")" \
    >"$(basename "$package_checksums")"
)

cleanup_release_package_working_directory
trap - EXIT
echo "$package_archive"

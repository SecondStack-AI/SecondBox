#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
evidence="$repo_root/.tmp/installer-qualification-evidence.json"
driver="$repo_root/scripts/installer-qualification-driver"
rm -f -- "$evidence"

[[ "${SECONDBOX_REQUIRE_QUALIFIED_INSTALLER:-}" == 1 ]] || {
  echo 'installer qualification requires SECONDBOX_REQUIRE_QUALIFIED_INSTALLER=1' >&2
  exit 1
}
: "${SECONDBOX_INSTALLER_RELEASE_DIRECTORY:?installer qualification requires a staged published-style release directory}"
: "${SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT:?installer qualification requires a dedicated existing XFS/Btrfs workspace parent}"
: "${SECONDBOX_INSTALLER_QUALIFICATION_IMAGE:?installer qualification requires an explicit pinned VM base image}"
: "${SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256:?installer qualification requires the VM base image SHA-256}"
[[ -x "$driver" ]] || { echo 'repository installer qualification driver is not executable' >&2; exit 1; }
for device in /dev/kvm /dev/net/tun; do
  [[ -c "$device" && -r "$device" && -w "$device" ]] || { echo "installer qualification requires readable/writable $device" >&2; exit 1; }
done
[[ -d "$SECONDBOX_INSTALLER_RELEASE_DIRECTORY" && ! -L "$SECONDBOX_INSTALLER_RELEASE_DIRECTORY" ]] || { echo 'installer qualification release directory is unsafe' >&2; exit 1; }
[[ -d "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT" && ! -L "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT" ]] || { echo 'installer qualification workspace parent is unsafe' >&2; exit 1; }

source_commit="$(git -C "$repo_root" rev-parse HEAD)"
[[ -z "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]] || { echo 'installer qualification requires a clean checkout' >&2; exit 1; }
mkdir -p "$repo_root/.tmp"
temporary="$(mktemp -d "$repo_root/.tmp/installer-qualified.XXXXXX")"
trap 'rm -rf "$temporary"; rm -f -- "$evidence"' ERR INT TERM
started="$(date +%s)"
mapfile -t release_manifests < <(find "$SECONDBOX_INSTALLER_RELEASE_DIRECTORY" -maxdepth 1 -type f -name 'secondbox-*-artifact-manifest.json' -print)
[[ "${#release_manifests[@]}" == 1 ]] || { echo 'installer qualification release directory must contain exactly one artifact manifest' >&2; exit 1; }
qualification_subject="$(go -C "$repo_root" run ./cmd/secondbox-release-tool installer-qualification-subject "${release_manifests[0]}")"
"$driver" run \
  --release-directory "$SECONDBOX_INSTALLER_RELEASE_DIRECTORY" \
  --existing-workspace-root "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT" \
  --scenario "$repo_root/tests/installer/vm-scenario.json" \
  --base-image "$SECONDBOX_INSTALLER_QUALIFICATION_IMAGE" \
  --base-image-sha256 "$SECONDBOX_INSTALLER_QUALIFICATION_IMAGE_SHA256" \
  --output "$temporary/driver-evidence.json"
finished="$(date +%s)"

jq -e --arg subject "$qualification_subject" --slurpfile scenario "$repo_root/tests/installer/vm-scenario.json" '
  .schemaVersion == "secondbox.install.qualified-driver-evidence/v1" and
  .passed == true and .rebootPassed == true and
  .releaseManifestDigest == $subject and
  (.filesystemIdentity | type == "string") and (.filesystemIdentity | length) > 0 and
  (($scenario[0].requiredAssertions - [.assertions[] | select(.passed == true) | .id]) | length) == 0 and
  all(.assertions[]; .passed == true)
' "$temporary/driver-evidence.json" >/dev/null
pass_count="$(jq -er '.assertions | length' "$temporary/driver-evidence.json")"
workspace_mount="$(findmnt -n -o TARGET --target "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT")"
workspace_type="$(findmnt -n -o FSTYPE --target "$SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT")"
[[ "$workspace_type" == xfs || "$workspace_type" == btrfs ]] || { echo 'installer qualification workspace must be XFS or Btrfs' >&2; exit 1; }
jq -n \
  --arg schemaVersion 'secondbox.release/installer-qualification-evidence/v1' \
  --arg sourceCommit "$source_commit" \
  --arg releaseManifestDigest "$(jq -er .releaseManifestDigest "$temporary/driver-evidence.json")" \
  --arg filesystemIdentity "$(jq -er .filesystemIdentity "$temporary/driver-evidence.json")" \
  --arg qualifiedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg workspaceMount "$workspace_mount" \
  --arg workspaceType "$workspace_type" \
  --argjson passCount "$pass_count" \
  --argjson wallClockSeconds "$((finished-started))" \
  '{schemaVersion:$schemaVersion,sourceCommit:$sourceCommit,repositoryDirty:false,suite:"test-installer-qualified",passCount:$passCount,wallClockSeconds:$wallClockSeconds,host:{kvm:{path:"/dev/kvm",present:true,readable:true,writable:true},tun:{path:"/dev/net/tun",present:true,readable:true,writable:true},workspaceFilesystem:{mount:$workspaceMount,type:$workspaceType}},releaseManifestDigest:$releaseManifestDigest,filesystemIdentity:$filesystemIdentity,rebootPassed:true,qualifiedAt:$qualifiedAt}' >"$evidence"
rm -rf "$temporary"
trap - ERR INT TERM
echo "SecondBox installer qualification passed: $evidence"

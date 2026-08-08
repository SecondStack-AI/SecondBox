#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
controller="${SECONDBOX_INSTALLER_VM_CONTROLLER:-}"
images_json="${SECONDBOX_INSTALLER_VM_IMAGES_JSON:-}"
if [[ -z "$controller" || -z "$images_json" ]]; then
  echo 'SecondBox installer VM suite skipped: set SECONDBOX_INSTALLER_VM_CONTROLLER and SECONDBOX_INSTALLER_VM_IMAGES_JSON.'
  exit 0
fi
[[ -x "$controller" ]] || { echo "installer VM controller is not executable: $controller" >&2; exit 1; }
jq -e 'type == "array" and length >= 4 and (map(.family) | contains(["debian","ubuntu","fedora","rolling"])) and all(.[]; (.image | type == "string") and (.image | length) > 0)' <<<"$images_json" >/dev/null

temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT
while IFS= read -r encoded; do
  family="$(base64 -d <<<"$encoded" | jq -er .family)"
  image="$(base64 -d <<<"$encoded" | jq -er .image)"
  evidence="$temporary/$family.json"
  "$controller" run \
    --family "$family" \
    --image "$image" \
    --scenario "$repo_root/tests/installer/vm-scenario.json" \
    --output "$evidence"
  jq -e --arg family "$family" --arg image "$image" --slurpfile scenario "$repo_root/tests/installer/vm-scenario.json" '
    .schemaVersion == "secondbox.install.vm-evidence/v1" and
    .family == $family and .image == $image and .passed == true and
    ([.assertions[] | select(.passed == true) | .id] | sort) == ($scenario[0].requiredAssertions | sort)
  ' "$evidence" >/dev/null
done < <(jq -r '.[] | @base64' <<<"$images_json")

echo 'SecondBox installer disposable-VM matrix passed.'

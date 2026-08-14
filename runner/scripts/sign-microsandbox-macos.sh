#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --bundle <runtime-directory> --identity <codesign-identity-or-dash>" >&2
}

bundle=""
identity=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --bundle)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      bundle="$2"
      shift 2
      ;;
    --identity)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      identity="$2"
      shift 2
      ;;
    *) usage; exit 2 ;;
  esac
done
[[ -n "$bundle" && -n "$identity" ]] || { usage; exit 2; }
[[ "$(uname -s)" == "Darwin" && "$(uname -m)" == "arm64" ]] || {
  echo "SecondBox macOS Microsandbox signing requires Apple Silicon macOS" >&2
  exit 1
}
[[ -d "$bundle" ]] || {
  echo "SecondBox macOS Microsandbox runtime bundle is missing: $bundle" >&2
  exit 1
}
bundle="$(cd -- "$bundle" && pwd -P)"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
entitlements="$script_dir/../deploy/microsandbox-hypervisor.entitlements.plist"
runner="$bundle/bin/secondbox-runner"
helper="$bundle/bin/secondbox-microsandbox-helper"
firmware="$bundle/lib/libkrunfw.5.dylib"
for path in "$runner" "$helper" "$firmware"; do
  [[ -f "$path" ]] || {
    echo "SecondBox macOS Microsandbox signing input is missing: $path" >&2
    exit 1
  }
done

codesign --force --sign "$identity" "$firmware"
codesign --force --sign "$identity" "$runner"
codesign --entitlements "$entitlements" --force --sign "$identity" "$helper"
if [[ -f "$bundle/bin/msb" ]]; then
  codesign --entitlements "$entitlements" --force --sign "$identity" "$bundle/bin/msb"
fi

codesign --verify --strict "$runner"
codesign --verify --strict "$helper"
codesign --verify --strict "$firmware"
effective_entitlements="$(codesign -d --entitlements :- "$helper" 2>&1)"
grep -q '<key>com.apple.security.hypervisor</key>' <<<"$effective_entitlements" || {
  echo "SecondBox macOS Microsandbox helper lacks the Hypervisor entitlement" >&2
  exit 1
}
grep -q '<key>com.apple.security.cs.disable-library-validation</key>' <<<"$effective_entitlements" || {
  echo "SecondBox macOS Microsandbox helper lacks library-validation entitlement" >&2
  exit 1
}
# The helper supplies the operator-pinned bundle path to libkrun at runtime. It must not acquire a
# second, mutable libkrunfw identity through a Mach-O load command.
linked_libraries="$(otool -L "$helper" | awk 'NR > 1 { print $1 }')"
if grep -q 'libkrunfw' <<<"$linked_libraries"; then
  echo "SecondBox macOS Microsandbox helper unexpectedly links libkrunfw globally" >&2
  exit 1
fi
if grep -Eq '/Users/|/opt/homebrew|/usr/local' <<<"$linked_libraries"; then
  echo "SecondBox macOS Microsandbox helper contains a mutable global runtime-library path" >&2
  exit 1
fi

printf '%s\n' "$effective_entitlements"

#!/usr/bin/env bash
set -euo pipefail

if [[ "$#" -ne 4 ]]; then
  echo "Usage: scripts/sign-release-package-manifest.sh MANIFEST.json PRIVATE_KEY SIGNATURE PUBLIC_KEY" >&2
  exit 2
fi

manifest_path="$(realpath -e -- "$1")"
private_key_path="$(realpath -e -- "$2")"
signature_path="$3"
public_key_path="$4"
if [[ -L "$1" || ! -f "$manifest_path" || -L "$2" || ! -f "$private_key_path" ]]; then
  echo "SecondBox release signing inputs must be regular non-symbolic-link files" >&2
  exit 1
fi
for output_path in "$signature_path" "$public_key_path"; do
  if [[ -e "$output_path" ]]; then
    echo "SecondBox release signing refuses to overwrite: $output_path" >&2
    exit 1
  fi
done
if ! jq -e '
  .schemaVersion == 1 and
  (.releaseVersion | type == "string" and length > 0) and
  (.sourceCommit | test("^[0-9a-f]{40}$")) and
  (.packageArchive.path | type == "string" and length > 0) and
  (.packageArchive.sha256 | test("^[0-9a-f]{64}$")) and
  (.packageArchive.size | type == "number" and . > 0) and
  (.files | type == "array" and length > 0)
' "$manifest_path" >/dev/null; then
  echo "SecondBox release signing requires a valid release package manifest" >&2
  exit 1
fi

openssl dgst -sha256 -sign "$private_key_path" -out "$signature_path" "$manifest_path"
openssl pkey -in "$private_key_path" -pubout -out "$public_key_path"
chmod 0644 "$signature_path" "$public_key_path"
openssl dgst -sha256 -verify "$public_key_path" -signature "$signature_path" "$manifest_path" >/dev/null

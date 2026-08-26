#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temporary="$(mktemp -d)"
trap 'rm -rf "$temporary"' EXIT

CGO_ENABLED=0 go -C "$repo_root" build -trimpath -o "$temporary/secondbox-deploy" ./cmd/secondbox-deploy
retired_environment="SECONDBOX_APPLICATION_"'AUTHORITIES_JSON'
retired_manifest_key="application_"'authorities_file'
retired_secret="application-"'authorities.json'
if rg -a -q "$retired_environment|$retired_manifest_key|$retired_secret" "$temporary/secondbox-deploy"; then
  echo 'unattended installer binary retained static application authority configuration' >&2
  exit 1
fi
set +e
"$temporary/secondbox-deploy" --output json --color never install --check >"$temporary/facts.json" 2>"$temporary/diagnostic"
status=$?
set -e
if [[ "$status" -ne 0 && "$status" -ne 2 ]]; then
  echo "unattended installer preflight exited $status" >&2
  sed -n '1,120p' "$temporary/diagnostic" >&2
  exit 1
fi
jq -e '
  .schemaVersion == "secondbox.install.host-facts/v1" and
  (.observedAt | type == "string") and
  (.hostIdentity | type == "string") and
  (.findings | type == "array") and (.findings | length) > 0
' "$temporary/facts.json" >/dev/null
if rg -q 'Bearer |privateKey|platform-token|"token"' "$temporary/facts.json" "$temporary/diagnostic"; then
  echo 'unattended preflight exposed a secret-bearing value' >&2
  exit 1
fi
if rg -q $'\033|\x1b\[' "$temporary/facts.json" "$temporary/diagnostic"; then
  echo 'unattended preflight emitted terminal controls' >&2
  exit 1
fi
printf 'SecondBox unattended preflight smoke passed (host classification exit %d).\n' "$status"

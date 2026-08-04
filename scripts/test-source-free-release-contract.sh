#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
suite="$repo_root/scripts/test-source-free-release.sh"
workflow="$repo_root/.github/workflows/release-finalize.yml"

bash -n "$suite"
if env -i PATH="$PATH" "$suite" >/dev/null 2>&1; then
  echo "source-free gate skipped absent required public inputs" >&2
  exit 1
fi
if rg -n 'git (clone|checkout)|go build|docker build|go run ./|\.git/' "$suite"; then
  echo "source-free suite contains a checkout or local build dependency" >&2
  exit 1
fi
rg -q 'Download the public qualification suite only' "$workflow"
rg -q 'needs: source-free-qualification' "$workflow"
rg -q 'release-index --manifest' "$workflow"
rg -q 'Publish qualification, then release index last' "$workflow"
if rg -n '^\s*(-\s+)?uses:\s*[^ ]+@(main|master|v[0-9]+([.]?[0-9]+)*)\s*$' "$workflow"; then
  echo "finalization workflow contains an unpinned external action" >&2
  exit 1
fi

go -C "$repo_root" test ./pkg/releaseverify ./pkg/releasefinalize ./cmd/secondbox-deploy -count=1

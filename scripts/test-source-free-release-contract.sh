#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
suite="$repo_root/scripts/test-source-free-release.sh"
qualifier="$repo_root/scripts/release-local-qualify.sh"
finalizer="$repo_root/scripts/release-local-finalize.sh"

bash -n "$suite"
bash -n "$qualifier"
bash -n "$finalizer"
if env -i PATH="$PATH" "$suite" >/dev/null 2>&1; then
  echo "source-free gate skipped absent required public inputs" >&2
  exit 1
fi
if rg -n 'git (clone|checkout)|go build|docker build|go run ./|\.git/' "$suite"; then
  echo "source-free suite contains a checkout or local build dependency" >&2
  exit 1
fi
rg -q 'releases/download' "$qualifier"
rg -q 'must be outside the source checkout' "$qualifier"
rg -q 'release-index --manifest' "$finalizer"
rg -q 'execution_node_unavailable' "$suite"
rg -q 'SecondBoxProblemError' "$suite"
rg -q 'errors.As' "$suite"
test "$(rg -c 'handle\.refresh|handle\.Refresh' "$suite")" -eq 2
test "$(rg -c 'SandboxStateStopped|\["stopped"\]' "$suite")" -eq 2
rg -Fq 'SecondBox/sdk/go/secondboxclient@v${version}' "$suite"
qualification_line="$(rg -n 'publish_exact "\$qualification"' "$finalizer" | cut -d: -f1)"
index_line="$(rg -n 'publish_exact "\$index"' "$finalizer" | cut -d: -f1)"
[[ "$qualification_line" -lt "$index_line" ]] || {
  echo "local finalization does not publish qualification before the release index" >&2
  exit 1
}
if rg -n 'git (clone|checkout)|go build|docker build|go run ./|\.git/' "$qualifier" "$suite"; then
  echo "local source-free qualification contains a checkout or local build dependency" >&2
  exit 1
fi

go -C "$repo_root" test ./pkg/releaseverify ./pkg/releasefinalize ./cmd/secondbox-deploy -count=1

#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# Deployment tooling and the privileged Runner each own this contract because
# the two Go modules deliberately share no dependency graph. Production source
# files must remain byte-identical; module-specific tests may differ.
control_plane="pkg/networkpolicycontract"
runner="runner/networkpolicycontract"

source_names() {
    find "$1" -maxdepth 1 -type f -name '*.go' ! -name '*_test.go' -printf '%f\n' | sort
}

control_plane_sources="$(source_names "$control_plane")"
runner_sources="$(source_names "$runner")"
[[ -n "$control_plane_sources" ]] || {
    echo "SecondBox logical-gateway contract has no production source: $control_plane" >&2
    exit 1
}
[[ "$control_plane_sources" == "$runner_sources" ]] || {
    diff -u <(printf '%s\n' "$control_plane_sources") <(printf '%s\n' "$runner_sources") || true
    echo "SecondBox logical-gateway contract source sets have diverged: $control_plane vs $runner" >&2
    exit 1
}

while IFS= read -r source; do
    cmp "$control_plane/$source" "$runner/$source" || {
        echo "SecondBox logical-gateway contract copies have diverged: $source" >&2
        exit 1
    }
done <<<"$control_plane_sources"

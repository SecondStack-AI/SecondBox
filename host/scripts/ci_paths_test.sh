#!/usr/bin/env bash
set -euo pipefail

host_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
monorepo_ci="$host_root/../../../.github/workflows/agent-manager-ci.yml"
hosted_network_gate='scripts/microvm-network-namespace-test.sh'

if ! grep -Eq "working-directory:[[:space:]]*apps/sandbox-service/host" "$monorepo_ci"; then
    echo "microVM network CI must run from the Sandbox Host boundary" >&2
    exit 1
fi
if ! grep -Eq "run:[[:space:]]*sudo[[:space:]]+$hosted_network_gate([[:space:]]|$)" "$monorepo_ci"; then
    echo "hosted CI must invoke $hosted_network_gate as root" >&2
    exit 1
fi

echo "hosted CI invokes the Sandbox Host network namespace gate"

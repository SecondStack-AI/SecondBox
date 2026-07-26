#!/usr/bin/env bash
set -euo pipefail

usage() {
    cat >&2 <<'USAGE'
Usage: scan-no-secrets.sh <rootfs-directory>

Fails if runtime credential material appears in paths that would be baked into a
golden microVM rootfs. This is a guardrail, not a full secret scanner.
USAGE
}

root="${1:-}"
if [ "${root:-}" = "-h" ] || [ "${root:-}" = "--help" ]; then
    usage
    exit 0
fi
if [ -z "$root" ] || [ ! -d "$root" ]; then
    usage
    exit 2
fi

for forbidden in \
    "$root/workspace/config/secrets.json" \
    "$root/workspace/config/auth.json" \
    "$root/runtime-private/env.json" \
    "$root/runtime-private/auth.json"; do
    if [ -e "$forbidden" ]; then
        echo "forbidden credential file in rootfs: ${forbidden#$root/}" >&2
        exit 1
    fi
done

patterns='(^[[:space:]]*-{5}BEGIN [A-Z ]*PRIVATE KEY-{5}|AGENT_PLATFORM_TOKEN=|AGENT_MANAGER_AGENT_RUNTIME_AUTH_SECRET=|xox[baprs]-[A-Za-z0-9-]{10,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,})'
scan_out="/tmp/agent-manager-microvm-secret-scan.$$"
if command -v rg >/dev/null 2>&1; then
    scan_cmd=(rg -n --hidden --no-ignore
        -g '!**/node_modules/**'
        -g '!**/.cache/**'
        -g '!**/__pycache__/**'
        -g '!*.map'
        -g '!*.so'
        -g '!*.so.*'
        -g '!*.a'
        -g '!*.o'
        "$patterns" "$root")
else
    scan_cmd=(grep -IREn
        --exclude-dir=node_modules
        --exclude-dir=.cache
        --exclude-dir=__pycache__
        --exclude='*.map'
        --exclude='*.so'
        --exclude='*.so.*'
        --exclude='*.a'
        --exclude='*.o'
        "$patterns" "$root")
fi
if "${scan_cmd[@]}" >"$scan_out" 2>/dev/null; then
    cat "$scan_out" >&2
    rm -f "$scan_out"
    exit 1
fi
rm -f "$scan_out"

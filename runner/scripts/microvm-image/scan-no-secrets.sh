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

patterns='(^[[:space:]]*-{5}BEGIN [A-Z ]*PRIVATE KEY-{5}|xox[baprs]-[A-Za-z0-9-]{10,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,})'
scan_out="/tmp/secondbox-runner-guest-secret-scan.$$"
if command -v rg >/dev/null 2>&1; then
    rg -n --hidden --no-ignore \
        -g '!**/node_modules/**' \
        -g '!**/.cache/**' \
        -g '!**/__pycache__/**' \
        -g '!**/opt/go/src/**/testdata/**' \
        -g '!**/opt/go/src/crypto/x509/platform_root_key.pem' \
        -g '!*.map' \
        -g '!*.so' \
        -g '!*.so.*' \
        -g '!*.a' \
        -g '!*.o' \
        "$patterns" "$root" >"$scan_out" 2>/dev/null || scan_status=$?
    if [ "${scan_status:-1}" -gt 1 ]; then
        rm -f "$scan_out"
        echo "secret scan failed while reading the rootfs" >&2
        exit 1
    fi
else
    while IFS= read -r -d '' candidate; do
        grep_status=0
        grep -IHEn "$patterns" "$candidate" >>"$scan_out" 2>/dev/null || grep_status=$?
        if [ "$grep_status" -gt 1 ]; then
            rm -f "$scan_out"
            echo "secret scan failed while reading ${candidate#$root/}" >&2
            exit 1
        fi
    done < <(
        find "$root" \
            \( -type d \( -name node_modules -o -name .cache -o -name __pycache__ \) \) -prune -o \
            \( -type d -path "$root/opt/go/src/*/testdata" \) -prune -o \
            \( -type f -path "$root/opt/go/src/crypto/x509/platform_root_key.pem" \) -prune -o \
            \( -type f \( -name '*.map' -o -name '*.so' -o -name '*.so.*' -o -name '*.a' -o -name '*.o' \) \) -prune -o \
            -type f -print0
    )
fi
if [ -s "$scan_out" ]; then
    cat "$scan_out" >&2
    rm -f "$scan_out"
    exit 1
fi
rm -f "$scan_out"

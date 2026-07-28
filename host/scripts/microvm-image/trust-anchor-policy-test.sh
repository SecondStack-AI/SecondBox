#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work_dir="$(mktemp -d)"
trap 'rm -rf "$work_dir"' EXIT

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work_dir/private.pem" >/dev/null 2>&1
openssl pkey -in "$work_dir/private.pem" -pubout -out "$work_dir/public.pem" >/dev/null 2>&1
fingerprint="$(openssl pkey -pubin -in "$work_dir/public.pem" -outform DER | sha256sum | awk '{print $1}')"

if "$script_dir/verify.sh" "$work_dir" "$work_dir/public.pem" "$(printf '0%.0s' {1..64})" >"$work_dir/out" 2>"$work_dir/err"; then
    echo "verify accepted the wrong trust-anchor fingerprint" >&2
    exit 1
fi
grep -q 'public key fingerprint mismatch' "$work_dir/err"

if "$script_dir/verify.sh" "$work_dir" "$work_dir/public.pem" invalid >"$work_dir/out" 2>"$work_dir/err"; then
    echo "verify accepted a malformed trust-anchor fingerprint" >&2
    exit 1
fi
grep -q 'expected public key fingerprint must be 64 lowercase hex characters' "$work_dir/err"

if SANDBOX_HOST_MICROVM_PUBLIC_KEY_SHA256="$fingerprint" "$script_dir/verify.sh" "$work_dir" >"$work_dir/out" 2>"$work_dir/err"; then
    echo "verify accepted a fingerprint pin without a trusted public key" >&2
    exit 1
fi
grep -q 'public key fingerprint pin requires a trusted public key' "$work_dir/err"

if "$script_dir/verify.sh" "$work_dir" "$work_dir/public.pem" "$fingerprint" >"$work_dir/out" 2>"$work_dir/err"; then
    echo "empty artifact directory unexpectedly verified" >&2
    exit 1
fi
grep -q 'missing artifact: kernel' "$work_dir/err"

echo "microVM trust-anchor policy checks passed"

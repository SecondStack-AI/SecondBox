#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

for required_file in \
    LICENSE \
    THIRD_PARTY_NOTICES.md \
    sdk/typescript/flue-runtime-beta9-LICENSE.txt \
    sdk/typescript/flue-runtime-beta9-source.json \
    runner/internal/firecracker/firecracker.lock \
    runner/scripts/microvm-image/kernel.lock \
    runner/scripts/microvm-image/rootfs/secondbox-debian-image-definition.json \
    runner/scripts/microvm-image/rootfs/secondbox-python-requirements.txt; do
    [[ -f "$required_file" ]] || {
        echo "SecondBox source asset policy missing $required_file" >&2
        exit 1
    }
done

if git ls-files runner/bin dist | grep -q .; then
    echo "SecondBox source asset policy forbids checked-in build outputs" >&2
    exit 1
fi

if git ls-files | grep -Eq '(^|/)(firecracker|jailer|kernel|rootfs\.ext4|shared\.img)$'; then
    echo "SecondBox source asset policy forbids checked-in execution binaries and disk images" >&2
    exit 1
fi

grep -q '^FIRECRACKER_VERSION=' runner/internal/firecracker/firecracker.lock
grep -q '^KERNEL_URL=https://cdn.kernel.org/' runner/scripts/microvm-image/kernel.lock
grep -Eq '^KERNEL_SHA256=[0-9a-f]{64}$' runner/scripts/microvm-image/kernel.lock

for upstream_notice in LICENSE NOTICE THIRD-PARTY; do
    grep -q "/out-${upstream_notice}" runner/Dockerfile || {
        echo "SecondBox runner image does not preserve Firecracker $upstream_notice" >&2
        exit 1
    }
done

if grep -Ev '^[[:space:]]*(#.*)?$|^[A-Za-z0-9_.-]+==[^[:space:]]+$' \
    runner/scripts/microvm-image/rootfs/secondbox-python-requirements.txt | grep -q .; then
    echo "SecondBox guest Python requirements contain an unpinned entry" >&2
    exit 1
fi

echo "SecondBox source asset and notice policy passed"

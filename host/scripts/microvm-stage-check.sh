#!/usr/bin/env bash
set -euo pipefail

host_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

static_only=false
if [ "${1:-}" = "--static" ]; then
    static_only=true
fi

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

warn() {
    echo "WARN: $*" >&2
}

fail_if_nodev_mount() {
    path="$1"
    [ -e "$path" ] || return 0
    options="$(findmnt -T "$path" -n -o OPTIONS 2>/dev/null || true)"
    case ",$options," in
        *,nodev,*) fail "$path is on a nodev mount; Firecracker jailer needs device nodes for /dev/net/tun and /dev/kvm" ;;
    esac
}

require_env() {
    key="$1"
    value="${!key:-}"
    [ -n "$value" ] || fail "$key is required"
}

cleanup_stale_smoke_firecrackers() {
    if command -v pkill >/dev/null 2>&1; then
        pkill -TERM -f '/firecracker --id fc-0123456789abcdef-' >/dev/null 2>&1 || true
        sleep 1
        pkill -KILL -f '/firecracker --id fc-0123456789abcdef-' >/dev/null 2>&1 || true
    fi
}

first_readable() {
    for path in "$@"; do
        if [ -r "$path" ]; then
            printf '%s\n' "$path"
            return 0
        fi
    done
    return 1
}

first_artifact_dir() {
    for path in "$@"; do
        if [ -r "$path/kernel" ] && [ -r "$path/rootfs.ext4" ]; then
            printf '%s\n' "$path"
            return 0
        fi
    done
    return 1
}

first_executable() {
    for path in "$@"; do
        if [ -x "$path" ]; then
            printf '%s\n' "$path"
            return 0
        fi
    done
    return 1
}

resolve_executable() {
    configured="$1"
    fallback="$2"
    if [ -n "$configured" ] && command -v "$configured" >/dev/null 2>&1; then
        command -v "$configured"
        return 0
    fi
    if [ -n "$configured" ] && [ -x "$configured" ]; then
        printf '%s\n' "$configured"
        return 0
    fi
    if [ -x "$fallback" ]; then
        printf '%s\n' "$fallback"
        return 0
    fi
    if [ -n "$configured" ]; then
        printf '%s\n' "$configured"
        return 0
    fi
    printf '%s\n' "$fallback"
}

newest_mise_go() {
    go_root="/home/${SUDO_USER:-$USER}/.local/share/mise/installs/go"
    [ -d "$go_root" ] || return 1
    find "$go_root" -mindepth 3 -maxdepth 3 -type f -path '*/bin/go' -perm /111 2>/dev/null | sort -V | tail -n 1
}

resolve_go() {
    system_go="$(command -v go 2>/dev/null || true)"
    if [ -n "$system_go" ] && [ -x "$system_go" ]; then
        case "$system_go" in
            */.local/share/mise/shims/go) ;;
            *)
                printf '%s\n' "$system_go"
                return 0
                ;;
        esac
    fi
    mise_go="$(newest_mise_go || true)"
    if [ -n "$mise_go" ] && [ -x "$mise_go" ]; then
        printf '%s\n' "$mise_go"
        return 0
    fi
    first_executable \
        /usr/local/go/bin/go \
        /usr/bin/go
}

default_config() {
    generated_dir="$(first_artifact_dir \
        "$host_root/releases/microvm/latest" \
        "$host_root/releases/microvm/smoketest" \
        "$host_root/tmp/microvm-image-local-smoke" || true)"
    smoke_dir="$host_root/tmp/firecracker-smoke"

    export SANDBOX_HOST_MICROVM_ALLOW_UNJAILED="${SANDBOX_HOST_MICROVM_ALLOW_UNJAILED:-false}"
    export SANDBOX_HOST_FIRECRACKER_PATH
    SANDBOX_HOST_FIRECRACKER_PATH="$(resolve_executable "${SANDBOX_HOST_FIRECRACKER_PATH:-firecracker}" "$smoke_dir/bin/firecracker")"
    export SANDBOX_HOST_FIRECRACKER_JAILER_PATH
    SANDBOX_HOST_FIRECRACKER_JAILER_PATH="$(resolve_executable "${SANDBOX_HOST_FIRECRACKER_JAILER_PATH:-jailer}" "$smoke_dir/bin/jailer")"
    export SANDBOX_HOST_MICROVM_KERNEL_PATH="${SANDBOX_HOST_MICROVM_KERNEL_PATH:-$(first_readable "$generated_dir/kernel" || true)}"
    export SANDBOX_HOST_MICROVM_ROOTFS_PATH="${SANDBOX_HOST_MICROVM_ROOTFS_PATH:-$(first_readable "$generated_dir/rootfs.ext4" || true)}"
    export SANDBOX_HOST_MICROVM_SHARED_IMAGE_PATH="${SANDBOX_HOST_MICROVM_SHARED_IMAGE_PATH:-$(first_readable "$generated_dir/shared.img" || true)}"
    export SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR="${SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR:-/var/lib/sandbox-host/jailer}"
    export SANDBOX_HOST_MICROVM_JAILER_UID="${SANDBOX_HOST_MICROVM_JAILER_UID:-${SUDO_UID:-$(id -u)}}"
    export SANDBOX_HOST_MICROVM_JAILER_GID="${SANDBOX_HOST_MICROVM_JAILER_GID:-${SUDO_GID:-$(id -g)}}"
    export SANDBOX_HOST_MICROVM_BRIDGE_NAME="${SANDBOX_HOST_MICROVM_BRIDGE_NAME:-agfc0}"
    export SANDBOX_HOST_MICROVM_BRIDGE_CIDR="${SANDBOX_HOST_MICROVM_BRIDGE_CIDR:-172.30.0.1/24}"
    export SANDBOX_HOST_MICROVM_GUEST_IP="${SANDBOX_HOST_MICROVM_GUEST_IP:-172.30.0.10}"
    export SANDBOX_HOST_MICROVM_TAP_PREFIX="${SANDBOX_HOST_MICROVM_TAP_PREFIX:-agfc}"
    export SANDBOX_HOST_MICROVM_JAILER_CGROUP_VERSION="${SANDBOX_HOST_MICROVM_JAILER_CGROUP_VERSION:-2}"
    export SANDBOX_HOST_MICROVM_JAILER_PARENT_CGROUP="${SANDBOX_HOST_MICROVM_JAILER_PARENT_CGROUP:-sandbox-host}"
    export SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT="${SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT:-18080}"
}

default_config

[ "$SANDBOX_HOST_MICROVM_ALLOW_UNJAILED" != "true" ] || fail "SANDBOX_HOST_MICROVM_ALLOW_UNJAILED must be false for staging"

require_env SANDBOX_HOST_FIRECRACKER_PATH
require_env SANDBOX_HOST_FIRECRACKER_JAILER_PATH
require_env SANDBOX_HOST_MICROVM_KERNEL_PATH
require_env SANDBOX_HOST_MICROVM_ROOTFS_PATH
require_env SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR
require_env SANDBOX_HOST_MICROVM_BRIDGE_NAME
require_env SANDBOX_HOST_MICROVM_BRIDGE_CIDR
require_env SANDBOX_HOST_MICROVM_GUEST_IP

[ -x "$SANDBOX_HOST_FIRECRACKER_PATH" ] || fail "SANDBOX_HOST_FIRECRACKER_PATH is not executable: $SANDBOX_HOST_FIRECRACKER_PATH"
[ -x "$SANDBOX_HOST_FIRECRACKER_JAILER_PATH" ] || fail "SANDBOX_HOST_FIRECRACKER_JAILER_PATH is not executable: $SANDBOX_HOST_FIRECRACKER_JAILER_PATH"
[ -r "$SANDBOX_HOST_MICROVM_KERNEL_PATH" ] || fail "SANDBOX_HOST_MICROVM_KERNEL_PATH is not readable: $SANDBOX_HOST_MICROVM_KERNEL_PATH"
[ -r "$SANDBOX_HOST_MICROVM_ROOTFS_PATH" ] || fail "SANDBOX_HOST_MICROVM_ROOTFS_PATH is not readable: $SANDBOX_HOST_MICROVM_ROOTFS_PATH"
if command -v debugfs >/dev/null 2>&1; then
    debugfs -R 'stat /init' "$SANDBOX_HOST_MICROVM_ROOTFS_PATH" 2>/dev/null | grep -q '^Inode:' || fail "SANDBOX_HOST_MICROVM_ROOTFS_PATH must be a generated microVM rootfs with /init: $SANDBOX_HOST_MICROVM_ROOTFS_PATH"
fi
sample_vsock_path="$SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR/firecracker/fc-0123456789abcdef-00000000/root/agent-manager.vsock"
[ "${#sample_vsock_path}" -lt 108 ] || fail "SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR is too long for Firecracker vsock Unix sockets: $SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR"
fail_if_nodev_mount "$SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR"

case "${SANDBOX_HOST_MICROVM_WORKSPACE_BACKEND:-ext4}" in
    ext4) ;;
    dm-thin)
        require_env SANDBOX_HOST_MICROVM_THIN_POOL_DEVICE
        [ -e "$SANDBOX_HOST_MICROVM_THIN_POOL_DEVICE" ] || fail "SANDBOX_HOST_MICROVM_THIN_POOL_DEVICE not found: $SANDBOX_HOST_MICROVM_THIN_POOL_DEVICE"
        command -v dmsetup >/dev/null 2>&1 || fail "dmsetup is required for dm-thin workspaces"
        ;;
    *) fail "SANDBOX_HOST_MICROVM_WORKSPACE_BACKEND must be ext4 or dm-thin" ;;
esac

if [ "$static_only" = true ]; then
    echo "microVM staging static checks passed"
    exit 0
fi

[ "$(id -u)" = "0" ] || fail "privileged staging validation must run as root"
mkdir -p "$SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR"
chown root:root "$SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR"
chmod 0755 "$SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR"
fail_if_nodev_mount "$SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR"
[ -e /dev/kvm ] || fail "/dev/kvm is missing"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || fail "/dev/kvm must be readable and writable"

for cmd in ip iptables mkfs.ext4; do
    command -v "$cmd" >/dev/null 2>&1 || fail "$cmd is required"
done
GO_BIN="$(resolve_go || true)"
[ -n "$GO_BIN" ] || fail "go is required; install Go or make it visible to the sudo user"
export PATH="$(dirname "$GO_BIN"):$PATH"

if ! ip link show "$SANDBOX_HOST_MICROVM_BRIDGE_NAME" >/dev/null 2>&1; then
    warn "bridge $SANDBOX_HOST_MICROVM_BRIDGE_NAME does not exist yet; the manager should create it from SANDBOX_HOST_MICROVM_BRIDGE_CIDR"
fi

iptables -t nat -S >/dev/null || fail "iptables NAT table is not accessible"

export SANDBOX_HOST_MICROVM_JAILED_NET_SMOKE=1
export SANDBOX_HOST_MICROVM_MEMORY_MIB="${SANDBOX_HOST_MICROVM_MEMORY_MIB:-2048}"
export SANDBOX_HOST_MICROVM_WORKSPACE_SIZE_MIB="${SANDBOX_HOST_MICROVM_WORKSPACE_SIZE_MIB:-256}"
export AGENT_MANAGER_EGRESS_PROXY_TRANSPARENT_HTTP_PORT="${AGENT_MANAGER_EGRESS_PROXY_TRANSPARENT_HTTP_PORT:-18080}"

cleanup_stale_smoke_firecrackers
(cd "$host_root" && "$GO_BIN" test ./internal/firecracker -run TestSmokeJailedTapAndTransparentRouteGeneratedImage -count=1 -v)

echo "microVM staging privileged checks passed"

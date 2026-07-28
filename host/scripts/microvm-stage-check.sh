#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

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

load_dotenv() {
    env_file="$repo_root/.env"
    if [ -f "$env_file" ]; then
        set -a
        # shellcheck disable=SC1090
        . "$env_file"
        set +a
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
        "$repo_root/releases/microvm/latest" \
        "$repo_root/releases/microvm/smoketest" \
        "$repo_root/tmp/microvm-image-local-smoke" || true)"
    smoke_dir="$repo_root/tmp/firecracker-smoke"

    export AGENT_MANAGER_MICROVM_ALLOW_UNJAILED="${AGENT_MANAGER_MICROVM_ALLOW_UNJAILED:-false}"
    export AGENT_MANAGER_FIRECRACKER_PATH
    AGENT_MANAGER_FIRECRACKER_PATH="$(resolve_executable "${AGENT_MANAGER_FIRECRACKER_PATH:-firecracker}" "$smoke_dir/bin/firecracker")"
    export AGENT_MANAGER_FIRECRACKER_JAILER_PATH
    AGENT_MANAGER_FIRECRACKER_JAILER_PATH="$(resolve_executable "${AGENT_MANAGER_FIRECRACKER_JAILER_PATH:-jailer}" "$smoke_dir/bin/jailer")"
    export AGENT_MANAGER_MICROVM_KERNEL_PATH="${AGENT_MANAGER_MICROVM_KERNEL_PATH:-$(first_readable "$generated_dir/kernel" || true)}"
    export AGENT_MANAGER_MICROVM_ROOTFS_PATH="${AGENT_MANAGER_MICROVM_ROOTFS_PATH:-$(first_readable "$generated_dir/rootfs.ext4" || true)}"
    export AGENT_MANAGER_MICROVM_SHARED_IMAGE_PATH="${AGENT_MANAGER_MICROVM_SHARED_IMAGE_PATH:-$(first_readable "$generated_dir/shared.img" || true)}"
    export AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR="${AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR:-/var/lib/agent-manager/jailer}"
    export AGENT_MANAGER_MICROVM_JAILER_UID="${AGENT_MANAGER_MICROVM_JAILER_UID:-${SUDO_UID:-$(id -u)}}"
    export AGENT_MANAGER_MICROVM_JAILER_GID="${AGENT_MANAGER_MICROVM_JAILER_GID:-${SUDO_GID:-$(id -g)}}"
    export AGENT_MANAGER_MICROVM_BRIDGE_NAME="${AGENT_MANAGER_MICROVM_BRIDGE_NAME:-agfc0}"
    export AGENT_MANAGER_MICROVM_BRIDGE_CIDR="${AGENT_MANAGER_MICROVM_BRIDGE_CIDR:-172.30.0.1/24}"
    export AGENT_MANAGER_MICROVM_GUEST_IP="${AGENT_MANAGER_MICROVM_GUEST_IP:-172.30.0.10}"
    export AGENT_MANAGER_MICROVM_TAP_PREFIX="${AGENT_MANAGER_MICROVM_TAP_PREFIX:-agfc}"
    export AGENT_MANAGER_MICROVM_JAILER_CGROUP_VERSION="${AGENT_MANAGER_MICROVM_JAILER_CGROUP_VERSION:-2}"
    export AGENT_MANAGER_MICROVM_JAILER_PARENT_CGROUP="${AGENT_MANAGER_MICROVM_JAILER_PARENT_CGROUP:-agent-manager}"
    export AGENT_MANAGER_EGRESS_PROXY_TRANSPARENT_HTTP_PORT="${AGENT_MANAGER_EGRESS_PROXY_TRANSPARENT_HTTP_PORT:-18080}"
}

load_dotenv
default_config

[ "$AGENT_MANAGER_MICROVM_ALLOW_UNJAILED" != "true" ] || fail "AGENT_MANAGER_MICROVM_ALLOW_UNJAILED must be false for staging"

require_env AGENT_MANAGER_FIRECRACKER_PATH
require_env AGENT_MANAGER_FIRECRACKER_JAILER_PATH
require_env AGENT_MANAGER_MICROVM_KERNEL_PATH
require_env AGENT_MANAGER_MICROVM_ROOTFS_PATH
require_env AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR
require_env AGENT_MANAGER_MICROVM_BRIDGE_NAME
require_env AGENT_MANAGER_MICROVM_BRIDGE_CIDR
require_env AGENT_MANAGER_MICROVM_GUEST_IP

[ -x "$AGENT_MANAGER_FIRECRACKER_PATH" ] || fail "AGENT_MANAGER_FIRECRACKER_PATH is not executable: $AGENT_MANAGER_FIRECRACKER_PATH"
[ -x "$AGENT_MANAGER_FIRECRACKER_JAILER_PATH" ] || fail "AGENT_MANAGER_FIRECRACKER_JAILER_PATH is not executable: $AGENT_MANAGER_FIRECRACKER_JAILER_PATH"
[ -r "$AGENT_MANAGER_MICROVM_KERNEL_PATH" ] || fail "AGENT_MANAGER_MICROVM_KERNEL_PATH is not readable: $AGENT_MANAGER_MICROVM_KERNEL_PATH"
[ -r "$AGENT_MANAGER_MICROVM_ROOTFS_PATH" ] || fail "AGENT_MANAGER_MICROVM_ROOTFS_PATH is not readable: $AGENT_MANAGER_MICROVM_ROOTFS_PATH"
if command -v debugfs >/dev/null 2>&1; then
    debugfs -R 'stat /init' "$AGENT_MANAGER_MICROVM_ROOTFS_PATH" 2>/dev/null | grep -q '^Inode:' || fail "AGENT_MANAGER_MICROVM_ROOTFS_PATH must be a generated microVM rootfs with /init: $AGENT_MANAGER_MICROVM_ROOTFS_PATH"
fi
sample_vsock_path="$AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR/firecracker/fc-0123456789abcdef-00000000/root/agent-manager.vsock"
[ "${#sample_vsock_path}" -lt 108 ] || fail "AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR is too long for Firecracker vsock Unix sockets: $AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR"
fail_if_nodev_mount "$AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR"

case "${AGENT_MANAGER_MICROVM_WORKSPACE_BACKEND:-ext4}" in
    ext4) ;;
    dm-thin)
        require_env AGENT_MANAGER_MICROVM_THIN_POOL_DEVICE
        [ -e "$AGENT_MANAGER_MICROVM_THIN_POOL_DEVICE" ] || fail "AGENT_MANAGER_MICROVM_THIN_POOL_DEVICE not found: $AGENT_MANAGER_MICROVM_THIN_POOL_DEVICE"
        command -v dmsetup >/dev/null 2>&1 || fail "dmsetup is required for dm-thin workspaces"
        ;;
    *) fail "AGENT_MANAGER_MICROVM_WORKSPACE_BACKEND must be ext4 or dm-thin" ;;
esac

if [ "$static_only" = true ]; then
    echo "microVM staging static checks passed"
    exit 0
fi

[ "$(id -u)" = "0" ] || fail "privileged staging validation must run as root"
mkdir -p "$AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR"
chown root:root "$AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR"
chmod 0755 "$AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR"
fail_if_nodev_mount "$AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR"
[ -e /dev/kvm ] || fail "/dev/kvm is missing"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || fail "/dev/kvm must be readable and writable"

for cmd in ip iptables mkfs.ext4; do
    command -v "$cmd" >/dev/null 2>&1 || fail "$cmd is required"
done
GO_BIN="$(resolve_go || true)"
[ -n "$GO_BIN" ] || fail "go is required; install Go or make it visible to the sudo user"
export PATH="$(dirname "$GO_BIN"):$PATH"

if ! ip link show "$AGENT_MANAGER_MICROVM_BRIDGE_NAME" >/dev/null 2>&1; then
    warn "bridge $AGENT_MANAGER_MICROVM_BRIDGE_NAME does not exist yet; the manager should create it from AGENT_MANAGER_MICROVM_BRIDGE_CIDR"
fi

iptables -t nat -S >/dev/null || fail "iptables NAT table is not accessible"

export AGENT_MANAGER_MICROVM_JAILED_NET_SMOKE=1
export AGENT_MANAGER_MICROVM_MEMORY_MIB="${AGENT_MANAGER_MICROVM_MEMORY_MIB:-2048}"
export AGENT_MANAGER_MICROVM_WORKSPACE_SIZE_MIB="${AGENT_MANAGER_MICROVM_WORKSPACE_SIZE_MIB:-256}"
export AGENT_MANAGER_EGRESS_PROXY_TRANSPARENT_HTTP_PORT="${AGENT_MANAGER_EGRESS_PROXY_TRANSPARENT_HTTP_PORT:-18080}"

cleanup_stale_smoke_firecrackers
"$GO_BIN" test ./internal/microvm -run TestSmokeJailedTapAndTransparentRouteGeneratedImage -count=1 -v

echo "microVM staging privileged checks passed"

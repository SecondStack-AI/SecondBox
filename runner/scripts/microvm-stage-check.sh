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

fail_if_incompatible_jail_mount() {
    path="$1"
    [ -e "$path" ] || return 0
    options="$(findmnt -T "$path" -n -o OPTIONS 2>/dev/null || true)"
    case ",$options," in
        *,nodev,*) fail "$path is on a nodev mount; Firecracker jailer needs device nodes for /dev/net/tun and /dev/kvm" ;;
        *,noexec,*) fail "$path is on a noexec mount; Firecracker jailer must execute Firecracker inside the jail" ;;
    esac
}

require_env() {
    key="$1"
    value="${!key-}"
    [ -n "$value" ] || fail "$key is required"
}

cleanup_stale_smoke_firecrackers() {
    if command -v pkill >/dev/null 2>&1; then
        pkill -TERM -f '/firecracker --id fc-0123456789abcdef-' >/dev/null 2>&1 || true
        sleep 1
        pkill -KILL -f '/firecracker --id fc-0123456789abcdef-' >/dev/null 2>&1 || true
    fi
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

newest_mise_go() {
    sudo_user="${SUDO_USER-}"
    if [ -z "$sudo_user" ]; then
        sudo_user="$(id -un)"
    fi
    go_root="/home/$sudo_user/.local/share/mise/installs/go"
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

require_env SECONDBOX_RUNNER_FIRECRACKER_PATH
require_env SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH
require_env SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH
require_env SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH
require_env SECONDBOX_RUNNER_FIRECRACKER_SHARED_IMAGE_PATH
require_env SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT
require_env SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_START
require_env SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_COUNT
require_env SECONDBOX_RUNNER_FIRECRACKER_JAILER_UID_ALLOW_BELOW_1000
require_env SECONDBOX_RUNNER_FIRECRACKER_JAILER_GID
require_env SECONDBOX_RUNNER_FIRECRACKER_CGROUP_VERSION
require_env SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT
require_env SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED
require_env SECONDBOX_RUNNER_FIRECRACKER_KERNEL_ARGS
require_env SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT
require_env SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT
require_env SECONDBOX_RUNNER_GUEST_HEARTBEAT_INTERVAL
require_env SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY
require_env SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256
require_env SECONDBOX_RUNNER_WORKSPACE_ROOT
require_env SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME
require_env SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR
require_env SECONDBOX_RUNNER_SANDBOX_GUEST_IP
require_env SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX
require_env SECONDBOX_RUNNER_SANDBOX_MAX_MEMORY_MIB
require_env SECONDBOX_RUNNER_SANDBOX_MAX_DISK_MIB
require_env SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT
require_env SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT
require_env SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT

case "$SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT:$SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT:$SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT" in
    *[!0-9:]*|:*|*:) fail "storage pressure thresholds must be explicit positive integers" ;;
esac
[ "$SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT" -gt 0 ] || fail "SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT must be positive"
[ "$SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT" -lt "$SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT" ] || fail "storage pressure recovery must be below warning"
[ "$SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT" -lt "$SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT" ] || fail "storage pressure warning must be below admission deny"
[ "$SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT" -lt 100 ] || fail "storage pressure admission deny must be below 100"

case "$SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT:$SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT" in
    *[!0-9:]*|:*|*:) fail "guest vsock ports must be explicit positive integers" ;;
esac
[ "$SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT" -gt 0 ] || fail "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT must be positive"
[ "$SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT" -gt 0 ] || fail "SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT must be positive"
[ "$SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT" -le 65535 ] || fail "SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT must be at most 65535"
[ "$SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT" -le 65535 ] || fail "SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT must be at most 65535"
[ "$SECONDBOX_RUNNER_GUEST_CONTROL_VSOCK_PORT" != "$SECONDBOX_RUNNER_GUEST_PROTOCOL_VSOCK_PORT" ] || fail "guest control and protocol vsock ports must be distinct"

[ "$SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED" != "true" ] || fail "SECONDBOX_RUNNER_FIRECRACKER_ALLOW_UNJAILED must be false for staging"

case "$SECONDBOX_RUNNER_WORKSPACE_ROOT" in
    /*) ;;
    *) fail "SECONDBOX_RUNNER_WORKSPACE_ROOT must be an absolute path" ;;
esac
[ "$SECONDBOX_RUNNER_WORKSPACE_ROOT" != "/" ] || fail "SECONDBOX_RUNNER_WORKSPACE_ROOT cannot be the filesystem root"
[ ! -L "$SECONDBOX_RUNNER_WORKSPACE_ROOT" ] || fail "SECONDBOX_RUNNER_WORKSPACE_ROOT cannot be a symbolic link"

[ -x "$SECONDBOX_RUNNER_FIRECRACKER_PATH" ] || fail "SECONDBOX_RUNNER_FIRECRACKER_PATH is not executable: $SECONDBOX_RUNNER_FIRECRACKER_PATH"
[ -x "$SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH" ] || fail "SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH is not executable: $SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH"
[ -r "$SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH" ] || fail "SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH is not readable: $SECONDBOX_RUNNER_FIRECRACKER_KERNEL_PATH"
[ -r "$SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH" ] || fail "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH is not readable: $SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"
if command -v debugfs >/dev/null 2>&1; then
    debugfs -R 'stat /init' "$SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH" 2>/dev/null | grep -q '^Inode:' || fail "SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH must be a generated microVM rootfs with /init: $SECONDBOX_RUNNER_FIRECRACKER_ROOTFS_PATH"
fi
sample_socket_path="$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT/firecracker/fc-abcd-01234567-compartment01234-01234567/root/firecracker.sock"
[ "${#sample_socket_path}" -lt 108 ] || fail "SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT is too long for maximum-length Firecracker API Unix sockets: $SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"
fail_if_incompatible_jail_mount "$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"

if [ "$static_only" = true ]; then
    echo "microVM staging static checks passed"
    exit 0
fi

[ "$(id -u)" = "0" ] || fail "privileged staging validation must run as root"
mkdir -p "$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"
chown root:root "$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"
chmod 0755 "$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"
fail_if_incompatible_jail_mount "$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"
mkdir -p "$SECONDBOX_RUNNER_WORKSPACE_ROOT"
[ -d "$SECONDBOX_RUNNER_WORKSPACE_ROOT" ] || fail "SECONDBOX_RUNNER_WORKSPACE_ROOT is not a directory"
[ -e /dev/kvm ] || fail "/dev/kvm is missing"
[ -r /dev/kvm ] && [ -w /dev/kvm ] || fail "/dev/kvm must be readable and writable"

for cmd in ip iptables mkfs.ext4; do
    command -v "$cmd" >/dev/null 2>&1 || fail "$cmd is required"
done
GO_BIN="$(resolve_go || true)"
[ -n "$GO_BIN" ] || fail "go is required; install Go or make it visible to the sudo user"
export PATH="$(dirname "$GO_BIN"):$PATH"

if ! ip link show "$SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME" >/dev/null 2>&1; then
    fail "bridge $SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME does not exist; install the standalone runner network policy before readiness"
fi

iptables -t nat -S >/dev/null || fail "iptables NAT table is not accessible"

export SECONDBOX_RUNNER_QUALIFY_JAILED_NETWORK=1
cleanup_stale_smoke_firecrackers
(cd "$host_root" && "$GO_BIN" test ./internal/firecracker -run TestSmokeJailedTapGeneratedImage -count=1 -v)

echo "microVM staging privileged checks passed"

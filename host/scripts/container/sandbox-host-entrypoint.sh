#!/usr/bin/env bash
set -euo pipefail

required_paths=(
    AGENT_MANAGER_MICROVM_WORKSPACE_DIR
    AGENT_MANAGER_MICROVM_RUN_DIR
    AGENT_MANAGER_MICROVM_LOG_DIR
    AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR
    AGENT_MANAGER_VM_LAUNCHER_STATE_DIR
    AGENT_MANAGER_MICROVM_LAUNCHER_SOCKET
    AGENT_MANAGER_VM_LAUNCHER_HARNESS_RESULT_DIR
    AGENT_MANAGER_HARNESS_SPOOL_DIR
    AGENT_MANAGER_LOG_DIR
)
for variable in "${required_paths[@]}"; do
    if [[ -z "${!variable:-}" || "${!variable}" != /* ]]; then
        echo "$variable must be an absolute path" >&2
        exit 2
    fi
done

install -d -o 10001 -g 10001 -m 0750 "$AGENT_MANAGER_MICROVM_WORKSPACE_DIR"
install -d -o 10001 -g 10001 -m 0750 "$AGENT_MANAGER_LOG_DIR"
install -d -o 0 -g 10001 -m 0770 "$AGENT_MANAGER_MICROVM_RUN_DIR"
install -d -o 0 -g 10001 -m 0750 "$(dirname "$AGENT_MANAGER_MICROVM_LAUNCHER_SOCKET")"
install -d -o 10001 -g 10001 -m 0750 "$AGENT_MANAGER_MICROVM_LOG_DIR"
install -d -o 0 -g 0 -m 0700 "$AGENT_MANAGER_MICROVM_JAILER_CHROOT_BASE_DIR" "$AGENT_MANAGER_VM_LAUNCHER_STATE_DIR"
install -d -o 0 -g 10001 -m 0750 "$AGENT_MANAGER_VM_LAUNCHER_HARNESS_RESULT_DIR"
install -d -o 10001 -g 10001 -m 0750 "$AGENT_MANAGER_HARNESS_SPOOL_DIR"

# systemd does not retain a container's process environment in its manager
# environment. Materialize only the reviewed launcher namespace for the unit.
environment_file=/run/agent-sandbox-host/launcher.env
umask 077
: > "$environment_file"
while IFS= read -r variable; do
    if [[ "$variable" == AGENT_MANAGER_* ]]; then
        printf '%s=%q\n' "$variable" "${!variable}" >> "$environment_file"
    fi
done < <(compgen -e | LC_ALL=C sort)

/usr/local/bin/microvm-host-network-setup apply

if [[ "$#" -ne 1 || "$1" != "/lib/systemd/systemd" ]]; then
    echo "sandbox host must start its private systemd as PID 1" >&2
    exit 2
fi

exec /lib/systemd/systemd

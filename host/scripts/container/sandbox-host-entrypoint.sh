#!/usr/bin/env bash
set -euo pipefail

required_paths=(
    AG_MICROVM_WORKSPACE_DIR
    AG_MICROVM_RUN_DIR
    AG_MICROVM_LOG_DIR
    AG_MICROVM_JAILER_CHROOT_BASE_DIR
    AG_VM_LAUNCHER_STATE_DIR
    AG_MICROVM_LAUNCHER_SOCKET
    AG_VM_LAUNCHER_HARNESS_RESULT_DIR
    AG_LOG_DIR
)
for variable in "${required_paths[@]}"; do
    if [[ -z "${!variable:-}" || "${!variable}" != /* ]]; then
        echo "$variable must be an absolute path" >&2
        exit 2
    fi
done

install -d -o 10001 -g 10001 -m 0750 "$AG_MICROVM_WORKSPACE_DIR"
install -d -o 10001 -g 10001 -m 0750 "$AG_LOG_DIR"
install -d -o 0 -g 10001 -m 0770 "$AG_MICROVM_RUN_DIR" "$(dirname "$AG_MICROVM_LAUNCHER_SOCKET")"
install -d -o 10001 -g 10001 -m 0750 "$AG_MICROVM_LOG_DIR"
install -d -o 0 -g 0 -m 0700 "$AG_MICROVM_JAILER_CHROOT_BASE_DIR" "$AG_VM_LAUNCHER_STATE_DIR"
install -d -o 0 -g 10001 -m 0770 "$AG_VM_LAUNCHER_HARNESS_RESULT_DIR"

if [[ "$#" -ne 1 || "$1" != "/lib/systemd/systemd" ]]; then
    echo "sandbox host must start its private systemd as PID 1" >&2
    exit 2
fi

exec /lib/systemd/systemd

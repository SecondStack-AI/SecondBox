#!/usr/bin/env bash
set -euo pipefail

required_paths=(
    SANDBOX_HOST_MICROVM_WORKSPACE_DIR
    SANDBOX_HOST_MICROVM_RUN_DIR
    SANDBOX_HOST_MICROVM_LOG_DIR
    SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR
    SANDBOX_HOST_LAUNCHER_STATE_DIR
    SANDBOX_HOST_LAUNCHER_SOCKET
    SANDBOX_HOST_LAUNCHER_HARNESS_STAGING_DIR
    SANDBOX_HOST_LAUNCHER_HARNESS_RESULT_DIR
    SANDBOX_HOST_HARNESS_SPOOL_DIR
    SANDBOX_HOST_LOG_DIR
)
for variable in "${required_paths[@]}"; do
    if [[ -z "${!variable:-}" || "${!variable}" != /* ]]; then
        echo "$variable must be an absolute path" >&2
        exit 2
    fi
done

install -d -o 10001 -g 10001 -m 0750 "$SANDBOX_HOST_MICROVM_WORKSPACE_DIR"
install -d -o 10001 -g 10001 -m 0750 "$SANDBOX_HOST_LOG_DIR"
install -d -o 0 -g 10001 -m 0770 "$SANDBOX_HOST_MICROVM_RUN_DIR"
install -d -o 0 -g 10001 -m 0750 "$(dirname "$SANDBOX_HOST_LAUNCHER_SOCKET")"
install -d -o 10001 -g 10001 -m 0700 "$SANDBOX_HOST_LAUNCHER_HARNESS_STAGING_DIR"
install -d -o 10001 -g 10001 -m 0750 "$SANDBOX_HOST_MICROVM_LOG_DIR"
install -d -o 0 -g 0 -m 0700 "$SANDBOX_HOST_MICROVM_JAILER_CHROOT_BASE_DIR" "$SANDBOX_HOST_LAUNCHER_STATE_DIR"
install -d -o 0 -g 10001 -m 0750 "$SANDBOX_HOST_LAUNCHER_HARNESS_RESULT_DIR"
install -d -o 10001 -g 10001 -m 0750 "$SANDBOX_HOST_HARNESS_SPOOL_DIR"

# systemd does not retain a container's process environment in its manager
# environment. Materialize only the reviewed launcher namespace for the unit.
environment_file=/run/sandbox-host/host.env
umask 077
: > "$environment_file"
while IFS= read -r variable; do
    if [[ "$variable" == AGENT_MANAGER_* || "$variable" == SANDBOX_HOST_* || "$variable" == INTEGRATION_SERVICE_INTERNAL_* ]]; then
        printf '%s=%q\n' "$variable" "${!variable}" >> "$environment_file"
    fi
done < <(compgen -e | LC_ALL=C sort)

/usr/local/bin/microvm-host-network-setup apply

if [[ "$#" -ne 1 || "$1" != "/lib/systemd/systemd" ]]; then
    echo "sandbox host must start its private systemd as PID 1" >&2
    exit 2
fi

exec /lib/systemd/systemd

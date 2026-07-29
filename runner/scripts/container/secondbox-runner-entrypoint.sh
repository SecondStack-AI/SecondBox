#!/usr/bin/env bash
set -euo pipefail

required_paths=(
    SECONDBOX_RUNNER_WORKSPACE_ROOT
    SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR
    SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR
    SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT
    SECONDBOX_RUNNER_LOG_DIR
)
for variable in "${required_paths[@]}"; do
    if [[ -z "${!variable:-}" || "${!variable}" != /* ]]; then
        echo "$variable must be an absolute path" >&2
        exit 2
    fi
done

install -d -o 10001 -g 10001 -m 0750 "$SECONDBOX_RUNNER_WORKSPACE_ROOT"
install -d -o 10001 -g 10001 -m 0750 "$SECONDBOX_RUNNER_LOG_DIR"
install -d -o 0 -g 0 -m 0700 \
    "$SECONDBOX_RUNNER_FIRECRACKER_RUN_DIR" \
    "$SECONDBOX_RUNNER_FIRECRACKER_LOG_DIR" \
    "$SECONDBOX_RUNNER_FIRECRACKER_JAIL_ROOT"

# systemd does not retain a container's process environment in its manager
# environment. Retain only the reviewed standalone runner namespace.
environment_file=/run/secondbox-runner/runner.env
umask 077
: > "$environment_file"
while IFS= read -r variable; do
    if [[ "$variable" == SECONDBOX_RUNNER_* ]]; then
        printf '%s=%q\n' "$variable" "${!variable}" >> "$environment_file"
    fi
done < <(compgen -e | LC_ALL=C sort)

/usr/local/bin/microvm-host-network-setup apply

if [[ "$#" -ne 1 || "$1" != "/lib/systemd/systemd" ]]; then
    echo "SecondBox Runner must start its private systemd as PID 1" >&2
    exit 2
fi

exec /lib/systemd/systemd

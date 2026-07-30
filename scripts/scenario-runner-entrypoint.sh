#!/usr/bin/env bash
set -euo pipefail
umask 077

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

/usr/local/bin/microvm-host-network-setup apply
exec /usr/local/bin/secondbox-runner

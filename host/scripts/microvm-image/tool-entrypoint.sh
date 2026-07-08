#!/bin/sh
set -eu

export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}"

if [ -w /proc/sys/kernel/unprivileged_userns_clone ]; then
    echo 1 > /proc/sys/kernel/unprivileged_userns_clone
fi

# Tool-executor microVMs run ONLY the in-guest tool-exec server (agentcy-microvm-agent).
# No runtime command is passed after the flags, so the agent serves /tool/exec over vsock
# for one dangerous operation and does NOT launch the full TypeScript agent runtime or the
# browser stack. The agent loop itself runs on the host as a Flue harness cell.
exec /usr/local/bin/agentcy-microvm-agent \
    --vsock-port "${AGENTCY_GUEST_VSOCK_PORT:-1024}" \
    --workspace "${AGENTCY_WORKSPACE_DIR:-/workspace}" \
    --runtime-private "${AGENTCY_RUNTIME_PRIVATE_DIR:-/runtime-private}" \
    --log "${AGENTCY_GUEST_LOG_PATH:-/tmp/agent-runtime.log}"

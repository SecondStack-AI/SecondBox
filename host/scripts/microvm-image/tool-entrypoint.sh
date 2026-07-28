#!/bin/sh
set -eu

export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}"

if [ -w /proc/sys/kernel/unprivileged_userns_clone ]; then
    echo 1 > /proc/sys/kernel/unprivileged_userns_clone
fi

# Tool-executor microVMs run only the Sandbox Host guest agent.
# No runtime command is passed after the flags, so the guest serves /tool/exec over vsock
# for one dangerous operation and does NOT launch the full TypeScript agent runtime or the
# browser stack. The agent loop itself runs on the host as a Flue harness cell.
exec /usr/local/bin/sandbox-guest-agent \
    --vsock-port "${AGENT_MANAGER_GUEST_VSOCK_PORT:-1024}" \
    --workspace "${AGENT_MANAGER_WORKSPACE_DIR:-/workspace}" \
    --runtime-private "${AGENT_MANAGER_RUNTIME_PRIVATE_DIR:-/runtime-private}" \
    --log "${AGENT_MANAGER_GUEST_LOG_PATH:-/tmp/agent-runtime.log}"

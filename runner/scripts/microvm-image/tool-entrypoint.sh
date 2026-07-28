#!/bin/sh
set -eu

export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}"

if [ -w /proc/sys/kernel/unprivileged_userns_clone ]; then
    echo 1 > /proc/sys/kernel/unprivileged_userns_clone
fi

kernel_arg() {
    name="$1"
    for field in $(cat /proc/cmdline); do
        case "$field" in
            "$name"=*)
                printf '%s\n' "${field#*=}"
                return 0
                ;;
        esac
    done
    return 1
}

instance_id="$(kernel_arg secondbox.instance_id)"
sandbox_id="$(kernel_arg secondbox.sandbox_id)"
sandbox_generation="$(kernel_arg secondbox.sandbox_generation)"
guest_build_id="$(kernel_arg secondbox.guest_build_id)"
image_manifest_digest="$(kernel_arg secondbox.image_manifest_digest)"
toolchain_manifest_digest="$(kernel_arg secondbox.toolchain_manifest_digest)"
control_vsock_port="$(kernel_arg secondbox.guest_control_vsock_port)"
protocol_vsock_port="$(kernel_arg secondbox.guest_protocol_vsock_port)"
heartbeat_interval="$(kernel_arg secondbox.guest_heartbeat_interval)"

: "${instance_id:?missing secondbox.instance_id kernel argument}"
: "${sandbox_id:?missing secondbox.sandbox_id kernel argument}"
: "${sandbox_generation:?missing secondbox.sandbox_generation kernel argument}"
: "${guest_build_id:?missing secondbox.guest_build_id kernel argument}"
: "${image_manifest_digest:?missing secondbox.image_manifest_digest kernel argument}"
: "${toolchain_manifest_digest:?missing secondbox.toolchain_manifest_digest kernel argument}"
: "${control_vsock_port:?missing secondbox.guest_control_vsock_port kernel argument}"
: "${protocol_vsock_port:?missing secondbox.guest_protocol_vsock_port kernel argument}"
: "${heartbeat_interval:?missing secondbox.guest_heartbeat_interval kernel argument}"

# The guest exposes legacy HTTP control and the independently-versioned canonical
# gRPC data plane on distinct, explicitly assigned vsock ports.
exec /usr/local/bin/secondbox-guest-agent \
    --control-vsock-port "$control_vsock_port" \
    --protocol-vsock-port "$protocol_vsock_port" \
    --workspace /workspace \
    --runtime-private /runtime-private \
    --log /tmp/secondbox-guest.log \
    --instance-id "$instance_id" \
    --sandbox-id "$sandbox_id" \
    --sandbox-generation "$sandbox_generation" \
    --guest-build-id "$guest_build_id" \
    --image-manifest-digest "$image_manifest_digest" \
    --toolchain-manifest-digest "$toolchain_manifest_digest" \
    --heartbeat-interval "$heartbeat_interval"

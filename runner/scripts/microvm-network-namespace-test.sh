#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "SecondBox runner network namespace test must run as root" >&2
  exit 1
fi
for command_name in ip iptables ip6tables python3 sysctl timeout; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "missing required command: $command_name" >&2
    exit 1
  }
done

runner_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
setup_script="$runner_root/scripts/microvm-host-network-setup.sh"
suffix="$(printf '%x' "$$")"
host_ns="sbhost-$suffix"
guest_ns="sbguest-$suffix"
bridge="sb${suffix:0:6}"
tap_prefix="sbt"
host_veth="${tap_prefix}${suffix:0:6}"
guest_veth="sbg${suffix:0:6}"
bridge_ip="198.18.200.1"
bridge_cidr="$bridge_ip/24"
guest_ip="198.18.200.2"
guest_cidr="198.18.200.0/24"
denied_port=15432
state_dir="/tmp/secondbox-runner-network-test-$suffix"
server_pid=""

network_setup() {
  ip netns exec "$host_ns" env \
    SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME="$bridge" \
    SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR="$bridge_cidr" \
    SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR="$guest_cidr" \
    SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX="$tap_prefix" \
    SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR="$state_dir/network-state" \
    SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE=true \
    "$setup_script" "$1"
}

cleanup() {
  status="$?"
  trap - EXIT
  set +e
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" >/dev/null 2>&1 || true
    wait "$server_pid" >/dev/null 2>&1 || true
  fi
  if ip netns list | awk '{print $1}' | grep -Fxq "$host_ns"; then
    network_setup remove >/dev/null 2>&1 || true
  fi
  ip netns delete "$guest_ns" >/dev/null 2>&1 || true
  ip netns delete "$host_ns" >/dev/null 2>&1 || true
  rm -rf "$state_dir"
  exit "$status"
}
trap cleanup EXIT

mkdir -p "$state_dir"
ip netns add "$host_ns"
ip netns add "$guest_ns"
ip -n "$host_ns" link set lo up
ip -n "$guest_ns" link set lo up
network_setup apply

ip link add "$host_veth" type veth peer name "$guest_veth"
ip link set "$host_veth" netns "$host_ns"
ip link set "$guest_veth" netns "$guest_ns"
ip -n "$host_ns" link set "$host_veth" master "$bridge"
ip -n "$host_ns" link set "$host_veth" up
ip -n "$guest_ns" addr add "$guest_ip/24" dev "$guest_veth"
ip -n "$guest_ns" link set "$guest_veth" up
ip -n "$guest_ns" route add default via "$bridge_ip"

ip netns exec "$host_ns" python3 - "$denied_port" <<'PY' &
import socket
import sys

with socket.socket() as listener:
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("0.0.0.0", int(sys.argv[1])))
    listener.listen()
    while True:
        connection, _ = listener.accept()
        connection.close()
PY
server_pid="$!"

for _ in $(seq 1 30); do
  if ip netns exec "$host_ns" python3 -c '
import socket
import sys
with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2):
    pass
' "$denied_port" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

if timeout 2 ip netns exec "$guest_ns" python3 -c '
import socket
import sys
with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1):
    pass
' "$bridge_ip" "$denied_port" >/dev/null 2>&1; then
  echo "SecondBox runner host-input policy allowed an unassigned port" >&2
  exit 1
fi

echo "SecondBox runner namespace policy passed: bridge/TAP path exists and host input defaults to deny"

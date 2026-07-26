#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "microVM network namespace test must run as root" >&2
  exit 1
fi

for command in ip iptables ip6tables python3 sysctl timeout; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "missing required command: $command" >&2
    exit 1
  fi
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
setup_script="$repo_root/scripts/microvm-host-network-setup.sh"
suffix="$(printf '%x' "$$")"
host_ns="aghost-$suffix"
guest_ns="agguest-$suffix"
bridge="ab${suffix:0:6}"
tap_prefix="agt"
host_veth="${tap_prefix}${suffix:0:6}"
guest_veth="agg${suffix:0:6}"
bridge_ip="198.18.200.1"
bridge_cidr="$bridge_ip/24"
guest_ip="198.18.200.2"
guest_cidr="198.18.200.0/24"
platform_port=18081
explicit_proxy_port=13128
transparent_proxy_port=18080
denied_port=15432
state_dir="${RUNNER_TEMP:-/tmp}/agent-manager-network-test-$suffix"
server_log="$state_dir/server.log"
server_pid=""

network_setup() {
  ip netns exec "$host_ns" env \
    AGENT_MANAGER_MICROVM_BRIDGE_NAME="$bridge" \
    AGENT_MANAGER_MICROVM_BRIDGE_CIDR="$bridge_cidr" \
    AGENT_MANAGER_MICROVM_GUEST_CIDR="$guest_cidr" \
    AGENT_MANAGER_MICROVM_TAP_PREFIX="$tap_prefix" \
    AGENT_MANAGER_EGRESS_PROXY_LISTEN_PORT="$explicit_proxy_port" \
    AGENT_MANAGER_EGRESS_PROXY_TRANSPARENT_HTTP_PORT="$transparent_proxy_port" \
    AGENT_MANAGER_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP=true \
    AGENT_MANAGER_PRIVATE_LISTEN_PORT="$platform_port" \
    AGENT_MANAGER_PRIVATE_IFACES="lo,$bridge,$host_veth" \
    AGENT_MANAGER_MICROVM_NETWORK_STATE_DIR="$state_dir/network-state" \
    AGENT_MANAGER_MICROVM_DELETE_BRIDGE=true \
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

ip netns exec "$host_ns" python3 - "$platform_port" "$explicit_proxy_port" \
  "$transparent_proxy_port" "$denied_port" >"$server_log" 2>&1 <<'PY' &
import selectors
import socket
import sys

selector = selectors.DefaultSelector()
for raw_port in sys.argv[1:]:
    port = int(raw_port)
    listener = socket.socket()
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("0.0.0.0", port))
    listener.listen()
    selector.register(listener, selectors.EVENT_READ, port)

while True:
    for key, _ in selector.select():
        connection, _ = key.fileobj.accept()
        with connection:
            connection.settimeout(1)
            connection.recv(1024)
            connection.sendall(f"port:{key.data}\n".encode())
PY
server_pid="$!"

request_port() {
  timeout 3 ip netns exec "$guest_ns" python3 -c '
import socket
import sys
with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1) as connection:
    connection.sendall(b"probe\n")
    print(connection.recv(1024).decode().strip())
' "$bridge_ip" "$1"
}

ready=false
for _ in $(seq 1 30); do
  if ip netns exec "$host_ns" python3 -c '
import socket
import sys
with socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=0.2) as connection:
    connection.sendall(b"ready\n")
    connection.recv(1024)
' "$platform_port" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.1
done
if [[ "$ready" != "true" ]]; then
  echo "test listeners did not become ready" >&2
  cat "$server_log" >&2
  exit 1
fi

if [[ "$(request_port "$platform_port")" != "port:$platform_port" ]]; then
  echo "sandbox platform allow rule did not pass traffic" >&2
  exit 1
fi
if [[ "$(request_port "$explicit_proxy_port")" != "port:$explicit_proxy_port" ]]; then
  echo "sandbox explicit-proxy allow rule did not pass traffic" >&2
  exit 1
fi
if [[ "$(request_port 80)" != "port:$transparent_proxy_port" ]]; then
  echo "sandbox HTTP traffic was not redirected to the transparent proxy" >&2
  exit 1
fi
if request_port "$denied_port" >/dev/null 2>&1; then
  echo "sandbox default-deny rule allowed an unapproved host port" >&2
  exit 1
fi

echo "microVM namespace policy passed: platform/proxy allow, HTTP redirect, default deny"

#!/usr/bin/env bash
set -Eeuo pipefail

mode="${1:-}"

usage() {
  cat <<'USAGE'
Usage:
  sudo env [VAR=...] scripts/microvm-host-network-setup.sh apply
  sudo env [VAR=...] scripts/microvm-host-network-setup.sh remove

Creates the standalone Firecracker bridge and installs a default-deny host
firewall baseline. Assignment-specific network policy is installed by the
runner, not by this host bootstrap.

Required environment:
  SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME
  SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR
  SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR
  SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX
  SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR
  SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE
USAGE
}

if [[ "$mode" == "-h" || "$mode" == "--help" ]]; then
  usage
  exit 0
fi
if [[ "$mode" != "apply" && "$mode" != "remove" ]]; then
  usage >&2
  exit 2
fi
if [[ "$(id -u)" != "0" ]]; then
  echo "must run as root" >&2
  exit 1
fi

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "$name is required" >&2
    exit 1
  fi
}

for name in \
  SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME \
  SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR \
  SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR \
  SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX \
  SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR \
  SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE; do
  require_env "$name"
done

bridge="$SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME"
bridge_cidr="$SECONDBOX_RUNNER_SANDBOX_BRIDGE_CIDR"
guest_cidr="$SECONDBOX_RUNNER_SANDBOX_GUEST_CIDR"
tap_prefix="$SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX"
tap_pattern="${tap_prefix}+"
network_state_dir="$SECONDBOX_RUNNER_SANDBOX_NETWORK_STATE_DIR"
delete_bridge="$SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE"
state_path="$network_state_dir/host-network.state"
ipv4_input_chain="SECONDBOX_SANDBOX_INPUT"
ipv4_forward_chain="SECONDBOX_SANDBOX_FORWARD"
ipv6_chain="SECONDBOX_SANDBOX_IPV6"

if [[ ! "$bridge" =~ ^[A-Za-z0-9_.-]{1,15}$ ]]; then
  echo "SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME must be a Linux interface name" >&2
  exit 1
fi
if [[ ! "$tap_prefix" =~ ^[A-Za-z0-9]{1,5}$ ]]; then
  echo "SECONDBOX_RUNNER_SANDBOX_TAP_PREFIX must be 1-5 alphanumeric characters" >&2
  exit 1
fi
if [[ "$delete_bridge" != "true" && "$delete_bridge" != "false" ]]; then
  echo "SECONDBOX_RUNNER_SANDBOX_DELETE_BRIDGE must be true or false" >&2
  exit 1
fi

for command_name in ip iptables ip6tables sysctl sync; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "missing required command: $command_name" >&2
    exit 1
  }
done

iptables_insert() {
  local chain="$1"
  shift
  iptables -t filter -C "$chain" "$@" >/dev/null 2>&1 ||
    iptables -t filter -I "$chain" 1 "$@"
}

iptables_add() {
  local chain="$1"
  shift
  iptables -t filter -C "$chain" "$@" >/dev/null 2>&1 ||
    iptables -t filter -A "$chain" "$@"
}

iptables_delete() {
  local chain="$1"
  shift
  while iptables -t filter -C "$chain" "$@" >/dev/null 2>&1; do
    iptables -t filter -D "$chain" "$@"
  done
}

ip6tables_insert() {
  local chain="$1"
  shift
  ip6tables -t filter -C "$chain" "$@" >/dev/null 2>&1 ||
    ip6tables -t filter -I "$chain" 1 "$@"
}

ip6tables_add() {
  local chain="$1"
  shift
  ip6tables -t filter -C "$chain" "$@" >/dev/null 2>&1 ||
    ip6tables -t filter -A "$chain" "$@"
}

ip6tables_delete() {
  local chain="$1"
  shift
  while ip6tables -t filter -C "$chain" "$@" >/dev/null 2>&1; do
    ip6tables -t filter -D "$chain" "$@"
  done
}

fingerprint() {
  printf '%s|%s|%s|%s|%s' "$bridge" "$bridge_cidr" "$guest_cidr" "$tap_pattern" "$delete_bridge"
}

read_state() {
  [[ -f "$state_path" ]] || return 1
  mapfile -t state_lines < "$state_path"
  if [[ "${#state_lines[@]}" != 5 ||
        ! "${state_lines[0]}" =~ ^previous_ip_forward=(0|1)$ ||
        ! "${state_lines[1]}" =~ ^bridge_created=(0|1)$ ||
        ! "${state_lines[2]}" =~ ^bridge_address_added=(0|1)$ ||
        ! "${state_lines[3]}" =~ ^status=(applying|active)$ ||
        ! "${state_lines[4]}" =~ ^config= ]]; then
    echo "invalid sandbox host-network state" >&2
    exit 1
  fi
  previous_ip_forward="${state_lines[0]#previous_ip_forward=}"
  bridge_created="${state_lines[1]#bridge_created=}"
  bridge_address_added="${state_lines[2]#bridge_address_added=}"
  state_status="${state_lines[3]#status=}"
  state_config="${state_lines[4]#config=}"
}

write_state() {
  local status="$1"
  local temporary="$state_path.tmp.$$"
  printf 'previous_ip_forward=%s\nbridge_created=%s\nbridge_address_added=%s\nstatus=%s\nconfig=%s\n' \
    "$previous_ip_forward" "$bridge_created" "$bridge_address_added" "$status" "$(fingerprint)" > "$temporary"
  chmod 0600 "$temporary"
  sync -f "$temporary"
  mv -f "$temporary" "$state_path"
  sync -f "$network_state_dir"
}

install_firewall() {
  iptables -N "$ipv4_input_chain" >/dev/null 2>&1 || true
  iptables -F "$ipv4_input_chain"
  iptables_add "$ipv4_input_chain" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  iptables_add "$ipv4_input_chain" -m comment --comment secondbox-runner-host-input-deny -j DROP
  for interface_name in "$bridge" "$tap_pattern"; do
    iptables_insert INPUT -i "$interface_name" -m comment --comment secondbox-runner-host-input -j "$ipv4_input_chain"
  done

  iptables -N "$ipv4_forward_chain" >/dev/null 2>&1 || true
  iptables -F "$ipv4_forward_chain"
  iptables_add "$ipv4_forward_chain" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  iptables_add "$ipv4_forward_chain" -m comment --comment secondbox-sandbox-forward-deny -j DROP
  for interface_name in "$bridge" "$tap_pattern"; do
    iptables_insert FORWARD -i "$interface_name" -m comment --comment secondbox-sandbox-forward-out -j "$ipv4_forward_chain"
    iptables_insert FORWARD -o "$interface_name" -m comment --comment secondbox-sandbox-forward-in -j "$ipv4_forward_chain"
  done

  ip6tables -N "$ipv6_chain" >/dev/null 2>&1 || true
  ip6tables -F "$ipv6_chain"
  ip6tables_add "$ipv6_chain" -m comment --comment secondbox-sandbox-ipv6-deny -j DROP
  for interface_name in "$bridge" "$tap_pattern"; do
    ip6tables_insert INPUT -i "$interface_name" -m comment --comment secondbox-sandbox-ipv6-input -j "$ipv6_chain"
    ip6tables_insert FORWARD -i "$interface_name" -m comment --comment secondbox-sandbox-ipv6-forward-out -j "$ipv6_chain"
    ip6tables_insert FORWARD -o "$interface_name" -m comment --comment secondbox-sandbox-ipv6-forward-in -j "$ipv6_chain"
  done
}

remove_firewall() {
  for interface_name in "$bridge" "$tap_pattern"; do
    iptables_delete INPUT -i "$interface_name" -m comment --comment secondbox-runner-host-input -j "$ipv4_input_chain"
    iptables_delete FORWARD -i "$interface_name" -m comment --comment secondbox-sandbox-forward-out -j "$ipv4_forward_chain"
    iptables_delete FORWARD -o "$interface_name" -m comment --comment secondbox-sandbox-forward-in -j "$ipv4_forward_chain"
    ip6tables_delete INPUT -i "$interface_name" -m comment --comment secondbox-sandbox-ipv6-input -j "$ipv6_chain"
    ip6tables_delete FORWARD -i "$interface_name" -m comment --comment secondbox-sandbox-ipv6-forward-out -j "$ipv6_chain"
    ip6tables_delete FORWARD -o "$interface_name" -m comment --comment secondbox-sandbox-ipv6-forward-in -j "$ipv6_chain"
  done
  iptables -F "$ipv4_input_chain" >/dev/null 2>&1 || true
  iptables -X "$ipv4_input_chain" >/dev/null 2>&1 || true
  iptables -F "$ipv4_forward_chain" >/dev/null 2>&1 || true
  iptables -X "$ipv4_forward_chain" >/dev/null 2>&1 || true
  ip6tables -F "$ipv6_chain" >/dev/null 2>&1 || true
  ip6tables -X "$ipv6_chain" >/dev/null 2>&1 || true
}

remove_network() {
  read_state || {
    echo "sandbox host network is not active"
    return 0
  }
  remove_firewall
  if [[ "$bridge_created" == "1" || "$delete_bridge" == "true" ]]; then
    ip link set "$bridge" down >/dev/null 2>&1 || true
    ip link delete "$bridge" type bridge >/dev/null 2>&1 || true
  elif [[ "$bridge_address_added" == "1" ]]; then
    ip addr del "$bridge_cidr" dev "$bridge" >/dev/null 2>&1 || true
  fi
  if [[ "$(sysctl -n net.ipv4.ip_forward)" != "$previous_ip_forward" ]]; then
    sysctl -w "net.ipv4.ip_forward=$previous_ip_forward" >/dev/null
  fi
  rm -f "$state_path"
  sync -f "$network_state_dir"
}

if [[ "$mode" == "remove" ]]; then
  remove_network
  exit 0
fi

install -d -m 0700 "$network_state_dir"
if read_state; then
  if [[ "$state_status" != "active" ]]; then
    remove_network
  elif [[ "$state_config" != "$(fingerprint)" ]]; then
    echo "active sandbox host-network config changed; remove it before applying new settings" >&2
    exit 1
  elif [[ "$(sysctl -n net.ipv4.ip_forward)" != "1" ]]; then
    echo "active sandbox host network lost net.ipv4.ip_forward=1" >&2
    exit 1
  else
    install_firewall
    exit 0
  fi
fi

previous_ip_forward="$(sysctl -n net.ipv4.ip_forward)"
bridge_created=0
bridge_address_added=0
if ! ip link show "$bridge" >/dev/null 2>&1; then
  bridge_created=1
  bridge_address_added=1
elif ! ip -4 addr show dev "$bridge" | grep -qw "$bridge_cidr"; then
  bridge_address_added=1
fi
write_state applying

rollback() {
  local status="$?"
  trap - ERR
  remove_network || true
  exit "$status"
}
trap rollback ERR

if [[ "$bridge_created" == "1" ]]; then
  ip link add name "$bridge" type bridge
fi
if [[ "$bridge_address_added" == "1" ]]; then
  ip addr add "$bridge_cidr" dev "$bridge"
fi
ip link set "$bridge" up
sysctl -w net.ipv4.ip_forward=1 >/dev/null
install_firewall
write_state active
trap - ERR

echo "configured standalone sandbox bridge $bridge ($bridge_cidr); guest forwarding defaults to deny"

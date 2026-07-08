#!/usr/bin/env bash
set -euo pipefail

mode="${1:-apply}"

if [[ "$mode" == "-h" || "$mode" == "--help" ]]; then
  cat <<'USAGE'
Usage:
  sudo env [VAR=...] scripts/microvm-host-network-setup.sh apply
  sudo env [VAR=...] scripts/microvm-host-network-setup.sh remove

Configures the host bridge and firewall prerequisites for the Firecracker
runtime. The agent manager still owns per-VM tap creation and per-instance
transparent HTTP redirect rules.

Environment:
  AG_MICROVM_BRIDGE_NAME                      bridge name, default agfc0
  AG_MICROVM_BRIDGE_CIDR                      bridge address, default 172.30.0.1/24
  AG_MICROVM_GUEST_CIDR                       guest subnet, default 172.30.0.0/24
  AG_EGRESS_PROXY_TRANSPARENT_HTTP_PORT       local transparent proxy port
  AG_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP    optional broad CIDR redirect, default false
  AG_MICROVM_ENABLE_DIRECT_NAT                optional direct outbound NAT, default false
  AG_MICROVM_EGRESS_IFACE                     outbound interface for direct NAT
  AG_MICROVM_DELETE_BRIDGE                    remove bridge in remove mode, default false
USAGE
  exit 0
fi

if [[ "$mode" != "apply" && "$mode" != "remove" ]]; then
  echo "unsupported mode: $mode" >&2
  exit 2
fi

if [[ "$(id -u)" != "0" ]]; then
  echo "must run as root; use sudo" >&2
  exit 1
fi

bridge="${AG_MICROVM_BRIDGE_NAME:-agfc0}"
bridge_cidr="${AG_MICROVM_BRIDGE_CIDR:-172.30.0.1/24}"
guest_cidr="${AG_MICROVM_GUEST_CIDR:-172.30.0.0/24}"
proxy_port="${AG_EGRESS_PROXY_TRANSPARENT_HTTP_PORT:-}"
install_cidr_redirect="${AG_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP:-false}"
direct_nat="${AG_MICROVM_ENABLE_DIRECT_NAT:-false}"
egress_iface="${AG_MICROVM_EGRESS_IFACE:-}"
delete_bridge="${AG_MICROVM_DELETE_BRIDGE:-false}"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

iptables_add() {
  local table="$1"
  shift
  if ! iptables -t "$table" -C "$@" >/dev/null 2>&1; then
    iptables -t "$table" -A "$@"
  fi
}

iptables_delete() {
  local table="$1"
  shift
  while iptables -t "$table" -C "$@" >/dev/null 2>&1; do
    iptables -t "$table" -D "$@"
  done
}

default_egress_iface() {
  ip route show default 0.0.0.0/0 | awk 'NR == 1 { for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit } }'
}

apply_config() {
  require_cmd ip
  require_cmd iptables
  require_cmd sysctl

  sysctl -w net.ipv4.ip_forward=1 >/dev/null

  if ! ip link show "$bridge" >/dev/null 2>&1; then
    ip link add name "$bridge" type bridge
  fi
  if ! ip -4 addr show dev "$bridge" | grep -qw "$bridge_cidr"; then
    ip addr add "$bridge_cidr" dev "$bridge"
  fi
  ip link set "$bridge" up

  if [[ -n "$proxy_port" ]]; then
    iptables_add filter INPUT -i "$bridge" -p tcp --dport "$proxy_port" -m comment --comment agentcy-microvm-proxy-input -j ACCEPT
  fi

  if [[ "$install_cidr_redirect" == "true" ]]; then
    if [[ -z "$proxy_port" ]]; then
      echo "AG_EGRESS_PROXY_TRANSPARENT_HTTP_PORT is required when AG_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP=true" >&2
      exit 1
    fi
    iptables_add nat PREROUTING -i "$bridge" -s "$guest_cidr" -p tcp --dport 80 -m comment --comment agentcy-microvm-egress:cidr -j REDIRECT --to-ports "$proxy_port"
  fi

  if [[ "$direct_nat" == "true" ]]; then
    if [[ -z "$egress_iface" ]]; then
      egress_iface="$(default_egress_iface)"
    fi
    if [[ -z "$egress_iface" ]]; then
      echo "could not determine egress interface; set AG_MICROVM_EGRESS_IFACE" >&2
      exit 1
    fi
    iptables_add nat POSTROUTING -s "$guest_cidr" -o "$egress_iface" -m comment --comment agentcy-microvm-direct-nat -j MASQUERADE
    iptables_add filter FORWARD -i "$bridge" -o "$egress_iface" -s "$guest_cidr" -m comment --comment agentcy-microvm-forward-out -j ACCEPT
    iptables_add filter FORWARD -i "$egress_iface" -o "$bridge" -d "$guest_cidr" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment agentcy-microvm-forward-in -j ACCEPT
  fi

  echo "configured $bridge ($bridge_cidr) for Firecracker microVM networking"
  echo "guest subnet: $guest_cidr"
  if [[ -n "$proxy_port" ]]; then
    echo "allowed transparent proxy input on tcp/$proxy_port from $bridge"
  fi
  if [[ "$install_cidr_redirect" == "true" ]]; then
    echo "installed optional CIDR HTTP redirect to tcp/$proxy_port"
  else
    echo "per-instance HTTP redirect remains owned by the agent manager"
  fi
  if [[ "$direct_nat" == "true" ]]; then
    echo "enabled optional direct NAT through $egress_iface"
  fi
}

remove_config() {
  require_cmd ip
  require_cmd iptables

  if [[ "$direct_nat" == "true" ]]; then
    if [[ -z "$egress_iface" ]]; then
      egress_iface="$(default_egress_iface || true)"
    fi
    if [[ -n "$egress_iface" ]]; then
      iptables_delete filter FORWARD -i "$egress_iface" -o "$bridge" -d "$guest_cidr" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment agentcy-microvm-forward-in -j ACCEPT
      iptables_delete filter FORWARD -i "$bridge" -o "$egress_iface" -s "$guest_cidr" -m comment --comment agentcy-microvm-forward-out -j ACCEPT
      iptables_delete nat POSTROUTING -s "$guest_cidr" -o "$egress_iface" -m comment --comment agentcy-microvm-direct-nat -j MASQUERADE
    fi
  fi

  if [[ "$install_cidr_redirect" == "true" && -n "$proxy_port" ]]; then
    iptables_delete nat PREROUTING -i "$bridge" -s "$guest_cidr" -p tcp --dport 80 -m comment --comment agentcy-microvm-egress:cidr -j REDIRECT --to-ports "$proxy_port"
  fi

  if [[ -n "$proxy_port" ]]; then
    iptables_delete filter INPUT -i "$bridge" -p tcp --dport "$proxy_port" -m comment --comment agentcy-microvm-proxy-input -j ACCEPT
  fi

  if [[ "$delete_bridge" == "true" && -d "/sys/class/net/$bridge" ]]; then
    ip link set "$bridge" down || true
    ip link delete "$bridge" type bridge || true
  fi

  echo "removed configured firewall rules for $bridge"
}

if [[ "$mode" == "apply" ]]; then
  apply_config
else
  remove_config
fi

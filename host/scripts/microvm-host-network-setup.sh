#!/usr/bin/env bash
set -Eeuo pipefail

mode="${1:-apply}"

if [[ "$mode" == "-h" || "$mode" == "--help" ]]; then
  cat <<'USAGE'
Usage:
  sudo env [VAR=...] scripts/microvm-host-network-setup.sh apply
  sudo env [VAR=...] scripts/microvm-host-network-setup.sh remove

Configures the host bridge and firewall prerequisites for the Firecracker
runtime. The root-owned vmlauncher owns per-VM tap creation and per-instance
transparent HTTP redirect rules; the network-facing manager remains unprivileged.

Environment:
  SANDBOX_HOST_MICROVM_BRIDGE_NAME                      bridge name, default agfc0
  SANDBOX_HOST_MICROVM_BRIDGE_CIDR                      bridge address, default 172.30.0.1/24
  SANDBOX_HOST_MICROVM_GUEST_CIDR                       guest subnet, default 172.30.0.0/24
  SANDBOX_HOST_MICROVM_TAP_PREFIX                       per-VM tap prefix, default agfc
  SANDBOX_HOST_EGRESS_PROXY_LISTEN_PORT                 explicit HTTP(S) proxy port, default 3128
  SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT       local transparent proxy port
  SANDBOX_HOST_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP    optional broad CIDR redirect, default false
  SANDBOX_HOST_MICROVM_ENABLE_DIRECT_NAT                optional direct outbound NAT, default false
  SANDBOX_HOST_MICROVM_EGRESS_IFACE                     outbound interface for direct NAT
  SANDBOX_HOST_MICROVM_DELETE_BRIDGE                    remove bridge in remove mode, default false
  SANDBOX_HOST_AGENT_SERVICE_PRIVATE_LISTEN_PORT              protected Agent Service port, default 8081
  SANDBOX_HOST_AGENT_SERVICE_PRIVATE_IFACES                   comma-separated allowed interfaces
  SANDBOX_HOST_MICROVM_NETWORK_STATE_DIR                root-owned rollback state directory
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

bridge="${SANDBOX_HOST_MICROVM_BRIDGE_NAME:-agfc0}"
bridge_cidr="${SANDBOX_HOST_MICROVM_BRIDGE_CIDR:-172.30.0.1/24}"
guest_cidr="${SANDBOX_HOST_MICROVM_GUEST_CIDR:-172.30.0.0/24}"
tap_prefix="${SANDBOX_HOST_MICROVM_TAP_PREFIX:-agfc}"
tap_pattern="${tap_prefix}+"
proxy_listen_port="${SANDBOX_HOST_EGRESS_PROXY_LISTEN_PORT:-3128}"
proxy_port="${SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT:-}"
install_cidr_redirect="${SANDBOX_HOST_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP:-false}"
direct_nat="${SANDBOX_HOST_MICROVM_ENABLE_DIRECT_NAT:-false}"
egress_iface="${SANDBOX_HOST_MICROVM_EGRESS_IFACE:-}"
delete_bridge="${SANDBOX_HOST_MICROVM_DELETE_BRIDGE:-false}"
agent_manager_port="${SANDBOX_HOST_AGENT_SERVICE_PRIVATE_LISTEN_PORT:-8081}"
agent_manager_ifaces="${SANDBOX_HOST_AGENT_SERVICE_PRIVATE_IFACES:-lo,$bridge,agh+}"
agent_manager_chain="SANDBOX_HOST_AGENT_IN"
sandbox_input_chain="SANDBOX_HOST_GUEST_IN"
harness_input_chain="SANDBOX_HOST_HARNESS_IN"
guest_forward_chain="SANDBOX_HOST_GUEST_FWD"
guest_forward_input_chain="SANDBOX_HOST_GUEST_FWD_IN"
sandbox_ipv6_input_chain="SANDBOX_HOST_GUEST6_IN"
sandbox_ipv6_forward_chain="SANDBOX_HOST_GUEST6_FWD"
network_state_dir="${SANDBOX_HOST_MICROVM_NETWORK_STATE_DIR:-/run/sandbox-host-guest-network}"
ip_forward_state_path="$network_state_dir/ip-forward.previous"
preserve_network_state=false
apply_recovery=false
active_reapply=false
active_reconcile=false
apply_mutated=false
previous_ip_forward=""
bridge_created=0
bridge_address_added=0
network_state_present=false
state_rule_config=""

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

set_ip_forward() {
  local desired="$1"
  if [[ "$(sysctl -n net.ipv4.ip_forward)" != "$desired" ]]; then
    sysctl -w "net.ipv4.ip_forward=$desired" >/dev/null
  fi
}

iptables_add() {
  local table="$1"
  shift
  if ! iptables -t "$table" -C "$@" >/dev/null 2>&1; then
    iptables -t "$table" -A "$@"
  fi
}

iptables_insert() {
  local table="$1"
  local chain="$2"
  shift 2
  if ! iptables -t "$table" -C "$chain" "$@" >/dev/null 2>&1; then
    iptables -t "$table" -I "$chain" 1 "$@"
  fi
}

iptables_delete() {
  local table="$1"
  shift
  while iptables -t "$table" -C "$@" >/dev/null 2>&1; do
    iptables -t "$table" -D "$@"
  done
}

ip6tables_insert() {
  local chain="$1"
  shift
  if ! ip6tables -t filter -C "$chain" "$@" >/dev/null 2>&1; then
    ip6tables -t filter -I "$chain" 1 "$@"
  fi
}

ip6tables_add() {
  local chain="$1"
  shift
  if ! ip6tables -t filter -C "$chain" "$@" >/dev/null 2>&1; then
    ip6tables -t filter -A "$chain" "$@"
  fi
}

ip6tables_delete() {
  local chain="$1"
  shift
  while ip6tables -t filter -C "$chain" "$@" >/dev/null 2>&1; do
    ip6tables -t filter -D "$chain" "$@"
  done
}

install_reconcile_guards() {
  iptables_insert filter INPUT -p tcp --dport "$agent_manager_port" -m comment --comment agent-manager-reconcile-private-guard -j DROP
  for iface in "$bridge" "$tap_pattern"; do
    iptables_insert filter INPUT -i "$iface" -m comment --comment agent-manager-reconcile-sandbox-input-guard -j DROP
  done
  iptables_insert filter INPUT -i 'agh+' -m comment --comment agent-manager-reconcile-harness-input-guard -j DROP
  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    iptables_insert filter FORWARD -i "$iface" -m comment --comment agent-manager-reconcile-forward-guard -j DROP
    iptables_insert filter FORWARD -o "$iface" -m comment --comment agent-manager-reconcile-forward-in-guard -j DROP
    ip6tables_insert INPUT -i "$iface" -m comment --comment agent-manager-reconcile-ipv6-input-guard -j DROP
    ip6tables_insert FORWARD -i "$iface" -m comment --comment agent-manager-reconcile-ipv6-forward-guard -j DROP
    ip6tables_insert FORWARD -o "$iface" -m comment --comment agent-manager-reconcile-ipv6-forward-in-guard -j DROP
  done
}

remove_reconcile_guards() {
  iptables_delete filter INPUT -p tcp --dport "$agent_manager_port" -m comment --comment agent-manager-reconcile-private-guard -j DROP
  for iface in "$bridge" "$tap_pattern"; do
    iptables_delete filter INPUT -i "$iface" -m comment --comment agent-manager-reconcile-sandbox-input-guard -j DROP
  done
  iptables_delete filter INPUT -i 'agh+' -m comment --comment agent-manager-reconcile-harness-input-guard -j DROP
  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    iptables_delete filter FORWARD -i "$iface" -m comment --comment agent-manager-reconcile-forward-guard -j DROP
    iptables_delete filter FORWARD -o "$iface" -m comment --comment agent-manager-reconcile-forward-in-guard -j DROP
    ip6tables_delete INPUT -i "$iface" -m comment --comment agent-manager-reconcile-ipv6-input-guard -j DROP
    ip6tables_delete FORWARD -i "$iface" -m comment --comment agent-manager-reconcile-ipv6-forward-guard -j DROP
    ip6tables_delete FORWARD -o "$iface" -m comment --comment agent-manager-reconcile-ipv6-forward-in-guard -j DROP
  done
}

default_egress_iface() {
  ip route show default 0.0.0.0/0 | awk 'NR == 1 { for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit } }'
}

validate_port() {
  local name="$1"
  local value="$2"
  if [[ ! "$value" =~ ^[0-9]+$ ]] || (( value < 1 || value > 65535 )); then
    echo "$name must be an integer between 1 and 65535" >&2
    exit 1
  fi
}

rule_config_fingerprint() {
  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s|%s' \
    "$bridge" "$bridge_cidr" "$guest_cidr" "$tap_pattern" "$proxy_listen_port" \
    "$proxy_port" "$install_cidr_redirect" "$direct_nat" "$egress_iface" \
    "$agent_manager_port" "$agent_manager_ifaces"
}

apply_rule_config() {
  IFS='|' read -r bridge bridge_cidr guest_cidr tap_pattern proxy_listen_port \
    proxy_port install_cidr_redirect direct_nat egress_iface agent_manager_port agent_manager_ifaces <<< "$1"
}

read_network_state() {
  [[ -f "$ip_forward_state_path" ]] || return 1
  mapfile -t state_lines < "$ip_forward_state_path"
  if [[ "${#state_lines[@]}" != 5 || ! "${state_lines[0]}" =~ ^previous=(0|1)$ || ! "${state_lines[1]}" =~ ^status=(applying|active)$ || ! "${state_lines[2]}" =~ ^bridge_created=(0|1)$ || ! "${state_lines[3]}" =~ ^bridge_address_added=(0|1)$ || ! "${state_lines[4]}" =~ ^config= ]]; then
    echo "invalid microVM network rollback state" >&2
    return 2
  fi
  previous_ip_forward="${state_lines[0]#previous=}"
  network_state_status="${state_lines[1]#status=}"
  bridge_created="${state_lines[2]#bridge_created=}"
  bridge_address_added="${state_lines[3]#bridge_address_added=}"
  state_rule_config="${state_lines[4]#config=}"
}

write_network_state() {
  local status="$1"
  local state_tmp="$ip_forward_state_path.tmp.$$"
  printf 'previous=%s\nstatus=%s\nbridge_created=%s\nbridge_address_added=%s\nconfig=%s\n' \
    "$previous_ip_forward" "$status" "$bridge_created" "$bridge_address_added" "$(rule_config_fingerprint)" > "$state_tmp"
  chmod 0600 "$state_tmp"
  sync -f "$state_tmp"
  mv -f "$state_tmp" "$ip_forward_state_path"
  sync -f "$network_state_dir"
}

apply_config() {
  require_cmd ip
  require_cmd iptables
  require_cmd ip6tables
  require_cmd sync
  require_cmd sysctl

  if [[ ! "$tap_prefix" =~ ^[A-Za-z0-9]{1,5}$ ]]; then
    echo "SANDBOX_HOST_MICROVM_TAP_PREFIX must be 1-5 alphanumeric characters to match launcher tap names" >&2
    exit 1
  fi
  validate_port SANDBOX_HOST_AGENT_SERVICE_PRIVATE_LISTEN_PORT "$agent_manager_port"
  validate_port SANDBOX_HOST_EGRESS_PROXY_LISTEN_PORT "$proxy_listen_port"
  if [[ -n "$proxy_port" ]]; then
    validate_port SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT "$proxy_port"
  fi
  if [[ "$install_cidr_redirect" == "true" && -z "$proxy_port" ]]; then
    echo "SANDBOX_HOST_EGRESS_PROXY_TRANSPARENT_HTTP_PORT is required when SANDBOX_HOST_MICROVM_INSTALL_CIDR_TRANSPARENT_HTTP=true" >&2
    return 1
  fi
  if [[ "$direct_nat" == "true" ]]; then
    if [[ -z "$egress_iface" ]]; then
      egress_iface="$(default_egress_iface)"
    fi
    if [[ -z "$egress_iface" ]]; then
      echo "could not determine egress interface; set SANDBOX_HOST_MICROVM_EGRESS_IFACE" >&2
      return 1
    fi
  fi

  desired_rule_config="$(rule_config_fingerprint)"
  install -d -m 0700 "$network_state_dir"
  if read_network_state; then
    if [[ "$network_state_status" == "active" ]]; then
      if [[ "$state_rule_config" != "$desired_rule_config" ]]; then
        echo "active microVM network policy config changed; run remove before applying new settings" >&2
        return 1
      fi
      active_reapply=true
      current_ip_forward="$(sysctl -n net.ipv4.ip_forward)"
      if [[ "$current_ip_forward" != "1" ]]; then
        echo "active microVM network policy has lost net.ipv4.ip_forward=1" >&2
        return 1
      fi
      echo "reconciling active microVM host network policy"
      active_reconcile=true
      active_reapply=false
    else
      apply_recovery=true
      preserve_network_state=true
      remove_config
      preserve_network_state=false
      apply_rule_config "$desired_rule_config"
      bridge_created=0
      bridge_address_added=0
      if ! ip link show "$bridge" >/dev/null 2>&1; then
        bridge_created=1
        bridge_address_added=1
      elif ! ip -4 addr show dev "$bridge" | grep -qw "$bridge_cidr"; then
        bridge_address_added=1
      fi
    fi
  else
    state_status="$?"
    if [[ "$state_status" != "1" ]]; then
      return "$state_status"
    fi
    previous_ip_forward="$(sysctl -n net.ipv4.ip_forward)"
    if [[ "$previous_ip_forward" != "0" && "$previous_ip_forward" != "1" ]]; then
      echo "unexpected net.ipv4.ip_forward value: $previous_ip_forward" >&2
      exit 1
    fi
    if ! ip link show "$bridge" >/dev/null 2>&1; then
      bridge_created=1
      bridge_address_added=1
    elif ! ip -4 addr show dev "$bridge" | grep -qw "$bridge_cidr"; then
      bridge_address_added=1
    fi
    write_network_state applying
  fi
  apply_mutated=true
  set_ip_forward 1

  if ! ip link show "$bridge" >/dev/null 2>&1; then
    ip link add name "$bridge" type bridge
  fi
  if ! ip -4 addr show dev "$bridge" | grep -qw "$bridge_cidr"; then
    ip addr add "$bridge_cidr" dev "$bridge"
  fi
  ip link set "$bridge" up

  if [[ "$active_reconcile" == "true" ]]; then
    install_reconcile_guards
  fi

  # Agent Service needs a bridge/private-mesh listener for guests and operators, but
  # the raw port must never become public. A dedicated chain installed at the
  # front of INPUT makes that policy explicit and independently removable.
  iptables -N "$agent_manager_chain" >/dev/null 2>&1 || true
  iptables -F "$agent_manager_chain"
  IFS=',' read -r -a private_ifaces <<< "$agent_manager_ifaces"
  for iface in "${private_ifaces[@]}"; do
    iface="${iface//[[:space:]]/}"
    [[ -n "$iface" ]] || continue
    iptables_insert filter "$agent_manager_chain" -i "$iface" -j ACCEPT
  done
  iptables_add filter "$agent_manager_chain" -j DROP
  if ! iptables -C INPUT -p tcp --dport "$agent_manager_port" -m comment --comment sandbox-host-agent-listener -j "$agent_manager_chain" >/dev/null 2>&1; then
    iptables -I INPUT 1 -p tcp --dport "$agent_manager_port" -m comment --comment sandbox-host-agent-listener -j "$agent_manager_chain"
  fi

  # Sandboxes may reach only the platform API and the two egress-proxy
  # listeners on the host. Hooks are interface-scoped, preserving unrelated
  # host INPUT policy while denying SSH, Postgres, and other host services.
  iptables -N "$sandbox_input_chain" >/dev/null 2>&1 || true
  iptables -F "$sandbox_input_chain"
  iptables_insert filter "$sandbox_input_chain" -p tcp --dport "$agent_manager_port" -m comment --comment sandbox-host-guest-platform -j ACCEPT
  iptables_insert filter "$sandbox_input_chain" -p tcp --dport "$proxy_listen_port" -m comment --comment sandbox-host-guest-explicit-proxy -j ACCEPT
  if [[ -n "$proxy_port" && "$proxy_port" != "$proxy_listen_port" ]]; then
    iptables_insert filter "$sandbox_input_chain" -p tcp --dport "$proxy_port" -m comment --comment sandbox-host-guest-transparent-proxy -j ACCEPT
  fi
  iptables_add filter "$sandbox_input_chain" -m comment --comment sandbox-host-guest-input-deny -j DROP
  for iface in "$bridge" "$tap_pattern"; do
    iptables_insert filter INPUT -i "$iface" -m comment --comment sandbox-host-guest-input -j "$sandbox_input_chain"
  done
  iptables -N "$harness_input_chain" >/dev/null 2>&1 || true
  iptables -F "$harness_input_chain"
  iptables_insert filter "$harness_input_chain" -p tcp --dport "$agent_manager_port" -m comment --comment sandbox-host-harness-platform -j ACCEPT
  iptables_insert filter "$harness_input_chain" -p tcp --dport "$proxy_listen_port" -m comment --comment sandbox-host-harness-explicit-proxy -j ACCEPT
  iptables_add filter "$harness_input_chain" -m comment --comment sandbox-host-harness-input-deny -j DROP
  iptables_insert filter INPUT -i 'agh+' -m comment --comment sandbox-host-harness-input -j "$harness_input_chain"

  # Production services are IPv4-only. Drop all IPv6 traffic arriving from or
  # forwarded out of sandbox interfaces, including link-local traffic.
  ip6tables -N "$sandbox_ipv6_input_chain" >/dev/null 2>&1 || true
  ip6tables -F "$sandbox_ipv6_input_chain"
  ip6tables_add "$sandbox_ipv6_input_chain" -m comment --comment sandbox-host-guest-ipv6-deny -j DROP
  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    ip6tables_insert INPUT -i "$iface" -m comment --comment sandbox-host-guest-ipv6-input -j "$sandbox_ipv6_input_chain"
  done

  ip6tables -N "$sandbox_ipv6_forward_chain" >/dev/null 2>&1 || true
  ip6tables -F "$sandbox_ipv6_forward_chain"
  ip6tables_add "$sandbox_ipv6_forward_chain" -m comment --comment sandbox-host-guest-ipv6-forward-deny -j DROP
  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    ip6tables_insert FORWARD -i "$iface" -m comment --comment sandbox-host-guest-ipv6-forward -j "$sandbox_ipv6_forward_chain"
    ip6tables_insert FORWARD -o "$iface" -m comment --comment sandbox-host-guest-ipv6-forward-in -j "$sandbox_ipv6_forward_chain"
  done

  if [[ "$install_cidr_redirect" == "true" ]]; then
    iptables_add nat PREROUTING -i "$bridge" -s "$guest_cidr" -p tcp --dport 80 -m comment --comment sandbox-host-guest-egress:cidr -j REDIRECT --to-ports "$proxy_port"
  fi

  if [[ "$direct_nat" == "true" ]]; then
    iptables_add nat POSTROUTING -s "$guest_cidr" -o "$egress_iface" -m comment --comment sandbox-host-guest-direct-nat -j MASQUERADE
  fi

  # Forwarding is denied for guest-originated packets unless direct NAT is an
  # explicit host policy. Match both the bridge master and derived tap names
  # because the observed ingress interface depends on bridge netfilter mode.
  iptables -N "$guest_forward_chain" >/dev/null 2>&1 || true
  iptables -F "$guest_forward_chain"
  if [[ "$direct_nat" == "true" ]]; then
    iptables_insert filter "$guest_forward_chain" -s "$guest_cidr" -o "$egress_iface" -m comment --comment sandbox-host-guest-forward-out -j ACCEPT
  fi
  iptables_add filter "$guest_forward_chain" -m comment --comment sandbox-host-guest-forward-deny -j DROP
  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    iptables_insert filter FORWARD -i "$iface" -m comment --comment sandbox-host-guest-forward-policy -j "$guest_forward_chain"
  done

  iptables -N "$guest_forward_input_chain" >/dev/null 2>&1 || true
  iptables -F "$guest_forward_input_chain"
  if [[ "$direct_nat" == "true" ]]; then
    iptables_insert filter "$guest_forward_input_chain" -i "$egress_iface" -d "$guest_cidr" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment sandbox-host-guest-forward-in -j ACCEPT
  fi
  iptables_add filter "$guest_forward_input_chain" -m comment --comment sandbox-host-guest-forward-in-deny -j DROP
  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    iptables_insert filter FORWARD -o "$iface" -m comment --comment sandbox-host-guest-forward-input-policy -j "$guest_forward_input_chain"
  done

  write_network_state active
  if [[ "$active_reconcile" == "true" ]]; then
    remove_reconcile_guards
  fi

  echo "configured $bridge ($bridge_cidr) for Firecracker microVM networking"
  echo "guest subnet: $guest_cidr"
  echo "protected Agent Service tcp/$agent_manager_port on interfaces: $agent_manager_ifaces"
  if [[ -n "$proxy_port" ]]; then
    echo "allowed transparent proxy input on tcp/$proxy_port from $bridge"
  fi
  if [[ "$install_cidr_redirect" == "true" ]]; then
    echo "installed optional CIDR HTTP redirect to tcp/$proxy_port"
  else
    echo "per-instance HTTP redirect remains owned by Sandbox Host"
  fi
  if [[ "$direct_nat" == "true" ]]; then
    echo "enabled optional direct NAT through $egress_iface"
  fi
}

remove_config() {
  require_cmd ip
  require_cmd iptables
  require_cmd ip6tables
  require_cmd sync
  require_cmd sysctl

  if read_network_state; then
    network_state_present=true
    apply_rule_config "$state_rule_config"
  else
    state_status="$?"
    if [[ "$state_status" != "1" ]]; then
      return "$state_status"
    fi
  fi
  remove_reconcile_guards

  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    ip6tables_delete FORWARD -o "$iface" -m comment --comment sandbox-host-guest-ipv6-forward-in -j "$sandbox_ipv6_forward_chain"
    ip6tables_delete FORWARD -i "$iface" -m comment --comment sandbox-host-guest-ipv6-forward -j "$sandbox_ipv6_forward_chain"
  done
  ip6tables -F "$sandbox_ipv6_forward_chain" >/dev/null 2>&1 || true
  ip6tables -X "$sandbox_ipv6_forward_chain" >/dev/null 2>&1 || true
  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    ip6tables_delete INPUT -i "$iface" -m comment --comment sandbox-host-guest-ipv6-input -j "$sandbox_ipv6_input_chain"
  done
  ip6tables -F "$sandbox_ipv6_input_chain" >/dev/null 2>&1 || true
  ip6tables -X "$sandbox_ipv6_input_chain" >/dev/null 2>&1 || true

  for iface in "$bridge" "$tap_pattern" 'agh+'; do
    iptables_delete filter FORWARD -o "$iface" -m comment --comment sandbox-host-guest-forward-input-policy -j "$guest_forward_input_chain"
    iptables_delete filter FORWARD -i "$iface" -m comment --comment sandbox-host-guest-forward-policy -j "$guest_forward_chain"
  done
  iptables -F "$guest_forward_input_chain" >/dev/null 2>&1 || true
  iptables -X "$guest_forward_input_chain" >/dev/null 2>&1 || true
  iptables -F "$guest_forward_chain" >/dev/null 2>&1 || true
  iptables -X "$guest_forward_chain" >/dev/null 2>&1 || true

  iptables_delete filter INPUT -i 'agh+' -m comment --comment sandbox-host-harness-input -j "$harness_input_chain"
  iptables -F "$harness_input_chain" >/dev/null 2>&1 || true
  iptables -X "$harness_input_chain" >/dev/null 2>&1 || true

  for iface in "$bridge" "$tap_pattern"; do
    iptables_delete filter INPUT -i "$iface" -m comment --comment sandbox-host-guest-input -j "$sandbox_input_chain"
  done
  iptables -F "$sandbox_input_chain" >/dev/null 2>&1 || true
  iptables -X "$sandbox_input_chain" >/dev/null 2>&1 || true

  while iptables -C INPUT -p tcp --dport "$agent_manager_port" -m comment --comment sandbox-host-agent-listener -j "$agent_manager_chain" >/dev/null 2>&1; do
    iptables -D INPUT -p tcp --dport "$agent_manager_port" -m comment --comment sandbox-host-agent-listener -j "$agent_manager_chain"
  done
  iptables -F "$agent_manager_chain" >/dev/null 2>&1 || true
  iptables -X "$agent_manager_chain" >/dev/null 2>&1 || true

  if [[ "$direct_nat" == "true" ]]; then
    if [[ -z "$egress_iface" ]]; then
      egress_iface="$(default_egress_iface || true)"
    fi
    if [[ -n "$egress_iface" ]]; then
      iptables_delete filter FORWARD -i "$egress_iface" -o "$bridge" -d "$guest_cidr" -m conntrack --ctstate ESTABLISHED,RELATED -m comment --comment sandbox-host-guest-forward-in -j ACCEPT
      iptables_delete filter FORWARD -i "$bridge" -o "$egress_iface" -s "$guest_cidr" -m comment --comment sandbox-host-guest-forward-out -j ACCEPT
      iptables_delete nat POSTROUTING -s "$guest_cidr" -o "$egress_iface" -m comment --comment sandbox-host-guest-direct-nat -j MASQUERADE
    fi
  fi

  if [[ "$install_cidr_redirect" == "true" && -n "$proxy_port" ]]; then
    iptables_delete nat PREROUTING -i "$bridge" -s "$guest_cidr" -p tcp --dport 80 -m comment --comment sandbox-host-guest-egress:cidr -j REDIRECT --to-ports "$proxy_port"
  fi
  if [[ -n "$proxy_port" ]]; then
    iptables_delete filter INPUT -i "$bridge" -p tcp --dport "$proxy_port" -m comment --comment sandbox-host-guest-proxy-input -j ACCEPT
  fi

  if [[ "$bridge_created" == "1" && -d "/sys/class/net/$bridge" ]]; then
    ip link set "$bridge" down
    ip link delete "$bridge" type bridge
  elif [[ "$bridge_address_added" == "1" ]]; then
    ip addr del "$bridge_cidr" dev "$bridge" >/dev/null 2>&1 || true
  elif [[ "$delete_bridge" == "true" && -d "/sys/class/net/$bridge" ]]; then
    ip link set "$bridge" down || true
    ip link delete "$bridge" type bridge || true
  fi

  if [[ "$network_state_present" == "true" && "$preserve_network_state" != "true" ]]; then
    set_ip_forward "$previous_ip_forward"
    rm -f "$ip_forward_state_path"
    sync -f "$network_state_dir"
    network_state_present=false
  fi

  echo "removed configured firewall rules for $bridge"
}

if [[ "$mode" == "apply" ]]; then
  rollback_apply() {
    status="$?"
    trap - ERR
    if [[ "$active_reapply" == "true" ]]; then
      exit "$status"
    fi
    if [[ "$apply_mutated" != "true" ]]; then
      exit "$status"
    fi
    set +e
    if [[ "$apply_recovery" == "true" ]]; then
      preserve_network_state=true
      remove_config
      set_ip_forward "$previous_ip_forward"
      write_network_state applying
    else
      remove_config
    fi
    set -e
    exit "$status"
  }
  trap rollback_apply ERR
  apply_config
  trap - ERR
else
  remove_config
fi

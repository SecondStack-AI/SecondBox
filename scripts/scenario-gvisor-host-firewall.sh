#!/usr/bin/env bash
set -euo pipefail
action="${1:?action}" image="${2:?runner image}" owner="${3:?suite project}" primary="${4:?primary profile}" relocation="${5:?relocation profile}"
[[ "$owner" =~ ^secondbox-suite-[0-9]+$ && "$primary" =~ ^([2-9]|1[0-4])$ && "$relocation" == "$((primary+1))" ]] || { echo 'SecondBox gVisor firewall requires an owned suite and reserved profile pair' >&2; exit 1; }
[[ "$action" == apply || "$action" == remove ]] || exit 2
nft() { docker run --rm --privileged --network host --interactive --entrypoint /usr/sbin/nft "$image" "$@"; }
ruleset="$(nft --json --handle list ruleset)"
# Docker's iptables-compatible host INPUT chain is optional on hosts without
# a host firewall. gVisor's earlier per-Instance policy remains authoritative.
if ! jq -e 'any(.nftables[]; .chain.family == "ip" and .chain.table == "filter" and .chain.name == "INPUT")' <<<"$ruleset" >/dev/null; then exit; fi
if [[ "$action" == apply ]]; then
  for profile in "$primary" "$relocation"; do
    printf 'insert rule ip filter INPUT iifname "gvh%s-*" ct mark 0x53425801 counter accept comment "%s"\n' "$profile" "$owner"
  done | nft --file -
else
  jq -r --arg owner "$owner" '.nftables[] | .rule? | select(.family == "ip" and .table == "filter" and .chain == "INPUT" and .comment == $owner) | "delete rule ip filter INPUT handle \(.handle)"' <<<"$ruleset" | nft --file -
fi

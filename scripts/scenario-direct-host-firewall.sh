#!/usr/bin/env bash
set -euo pipefail

action="${1:?action}" image="${2:?runner image}" owner="${3:?suite project}"
subnet="${4:?Compose subnet}" gateway="${5:?Compose gateway}"
primary="${6:?primary data-plane port}" relocation="${7:?relocation data-plane port}"
[[ "$owner" =~ ^secondbox-(suite|stress|lifecycle)-[0-9]+$ &&
   "$subnet" =~ ^198\.(18|19)\.([0-9]{1,3})\.0/24$ ]] || exit 2
octet="${BASH_REMATCH[2]}"
[[ "$octet" -le 255 && "$gateway" == "${subnet%.0/24}.1" ]] || exit 2
for port in "$primary" "$relocation"; do
  [[ "$port" =~ ^[1-9][0-9]{0,4}$ && "$port" -le 65535 ]] || exit 2
done
[[ "$action" == apply || "$action" == remove ]] || exit 2
nft() {
  docker run --rm --name "$owner-direct-firewall" --privileged --network host --interactive \
    --entrypoint /usr/sbin/nft "$image" "$@"
}
ruleset="$(nft --json --handle list ruleset)"
# Bridge-to-host traffic traverses INPUT, not Docker's published-port FORWARD
# rules. Hosts without this iptables-compatible firewall need no exception.
if ! jq -e 'any(.nftables[]; .chain.family == "ip" and .chain.table == "filter" and .chain.name == "INPUT")' <<<"$ruleset" >/dev/null; then exit; fi
comment="$owner-direct-data-plane"
if [[ "$action" == apply ]]; then
  printf 'insert rule ip filter INPUT ip saddr %s ip daddr %s tcp dport { %s, %s } counter accept comment "%s"\n' \
    "$subnet" "$gateway" "$primary" "$relocation" "$comment" | nft --file -
else
  jq -r --arg owner "$comment" '.nftables[] | .rule? | select(.family == "ip" and .table == "filter" and .chain == "INPUT" and .comment == $owner) | "delete rule ip filter INPUT handle \(.handle)"' <<<"$ruleset" | nft --file -
fi

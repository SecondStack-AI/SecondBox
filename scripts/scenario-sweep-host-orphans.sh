#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# Reclaim the per-run host resources that scenario suite runs left behind.
#
# `scripts/test-scenario.sh` names one bridge and one jailer cgroup parent per
# run from its own PID and releases both from its EXIT trap. A run killed with
# SIGKILL never reaches that trap, so without a sweep the host accumulates one
# dead bridge and one dead cgroup tree per killed run forever, and a live run's
# resources become indistinguishable from dead ones.
#
# The sweep is deliberately narrow. It considers only names in the suite's own
# scheme, and it never removes a name a running container still declares. The
# runner devnet bridge, Docker bridges, and every other host interface are
# outside the scheme and are never candidates.
#
# The qualification host has no passwordless sudo, so removal runs in the same
# privileged containers the suite already uses for host namespace work.

image="${SECONDBOX_SCENARIO_SWEEP_IMAGE:?SecondBox scenario orphan sweep requires SECONDBOX_SCENARIO_SWEEP_IMAGE}"
cgroup_root="${SECONDBOX_SCENARIO_SWEEP_CGROUP_ROOT:-/sys/fs/cgroup}"

# The suite's own naming scheme, stated once here and revalidated inside the
# privileged containers that perform the removals. A run derives both interface
# names from its PID: the bridge from `pid % 100000` and the TAP prefix from
# `pid % 1000`, so a suite bridge's own TAP prefix follows from its name.
bridge_pattern='^sbxq[0-9]+$'
interface_pattern='^(sbxq[0-9]+|sq[0-9a-f]+)$'
cgroup_pattern='^secondbox-scenario-[0-9]+$'
# Bounded reporting: a first sweep of a long-lived host resolves hundreds of
# names and the run log must stay readable.
report_limit=20

fail() {
  echo "SecondBox scenario orphan sweep failed: $*" >&2
  exit 1
}

report() {
  local action="$1"
  local kind="$2"
  shift 2
  local total="$#"
  (( total > 0 )) || return 0
  local shown="$*"
  if (( total > report_limit )); then
    shown="${*:1:report_limit} (and $(( total - report_limit )) more)"
  fi
  echo "SecondBox scenario orphan sweep $action $kind ($total): $shown"
}

report_kept() {
  local kind="$1"
  shift
  local entry
  for entry in "${@:1:report_limit}"; do
    echo "SecondBox scenario orphan sweep kept $kind: $entry"
  done
  if (( $# > report_limit )); then
    echo "SecondBox scenario orphan sweep kept $(( $# - report_limit )) further live $kind"
  fi
}

for command in awk docker ip sed sort; do
  command -v "$command" >/dev/null 2>&1 || fail "missing command: $command"
done
[[ "$cgroup_root" = /* && -d "$cgroup_root" ]] ||
  fail "SECONDBOX_SCENARIO_SWEEP_CGROUP_ROOT must be an existing absolute directory: $cgroup_root"

# A name a running container still declares is live whatever its state on the
# host. A container that stops between the listing and the inspection no longer
# holds a host resource, so inspecting the survivors is authoritative for the
# ones that remain.
declare -A declared_by_running_container=()
mapfile -t running_containers < <(docker ps --quiet)
if (( ${#running_containers[@]} > 0 )); then
  while IFS= read -r declaration; do
    declared_by_running_container["${declaration#*=}"]=1
  done < <(
    docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' \
      "${running_containers[@]}" 2>/dev/null |
      grep -E '^(SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME|SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT)=' ||
      true
  )
fi

kept_bridges=()
# TAPs precede the bridge they were enslaved to, because deleting a bridge only
# unenslaves its members and would leave them behind as loose interfaces.
removed_interfaces=()
mapfile -t host_bridges < <(
  ip -o link show type bridge |
    awk -F': ' '{print $2}' |
    sed 's/@.*//' |
    sort -u
)
for bridge in ${host_bridges[@]+"${host_bridges[@]}"}; do
  [[ "$bridge" =~ $bridge_pattern ]] || continue
  if [[ -n "${declared_by_running_container[$bridge]:-}" ]]; then
    kept_bridges+=("$bridge is declared by a running container")
    continue
  fi
  # A run killed mid-flight leaves its own TAPs enslaved. They belong to the
  # same dead run as the bridge, so they are reclaimed with it — but anything
  # outside that run's derived TAP scheme keeps the whole bridge.
  bridge_index="${bridge#sbxq}"
  bridge_tap_prefix="sq$(( bridge_index % 1000 ))"
  mapfile -t enslaved < <(
    ip -o link show master "$bridge" 2>/dev/null |
      awk -F': ' '{print $2}' |
      sed 's/@.*//'
  )
  foreign_interface=""
  for interface in ${enslaved[@]+"${enslaved[@]}"}; do
    [[ "$interface" =~ ^${bridge_tap_prefix}[0-9a-f]+$ ]] || foreign_interface="$interface"
  done
  if [[ -n "$foreign_interface" ]]; then
    kept_bridges+=("$bridge has an enslaved interface outside its own scheme: $foreign_interface")
    continue
  fi
  removed_interfaces+=(${enslaved[@]+"${enslaved[@]}"} "$bridge")
done

if (( ${#removed_interfaces[@]} > 0 )); then
  docker run --rm --privileged --network host \
    --entrypoint /bin/bash "$image" -c '
      set -Eeuo pipefail
      pattern="$1"
      shift
      for name; do
        [[ "$name" =~ $pattern ]] ||
          { echo "orphan sweep refused to remove interface $name" >&2; exit 1; }
        # An interface whose owning run tore it down between the host listing
        # and this delete needs nothing done; any other failure is reported.
        ip link show "$name" >/dev/null 2>&1 || continue
        ip link delete "$name"
      done
    ' scenario-orphan-sweep "$interface_pattern" "${removed_interfaces[@]}" ||
    fail "could not remove ${#removed_interfaces[@]} orphan interfaces"
fi

candidate_parents=()
removed_parents=()
kept_parents=()
for directory in "$cgroup_root"/*; do
  [[ -d "$directory" ]] || continue
  parent="${directory##*/}"
  [[ "$parent" =~ $cgroup_pattern ]] || continue
  if [[ -n "${declared_by_running_container[$parent]:-}" ]]; then
    kept_parents+=("$parent is declared by a running container")
    continue
  fi
  candidate_parents+=("$parent")
done

if (( ${#candidate_parents[@]} > 0 )); then
  # Only root may descend into a jailer cgroup tree, and the kernel refuses to
  # remove a cgroup that still has members or live children. That refusal is the
  # authority on what is in use, so the attempt and the verdict are one step
  # inside the privileged container: a tree that survives the attempt is live.
  cgroup_verdicts="$(
    docker run --rm --privileged --cgroupns=host --network none \
      --volume "$cgroup_root:/sys/fs/cgroup:rw" \
      --entrypoint /bin/bash "$image" -c '
        set -Eeuo pipefail
        pattern="$1"
        shift
        for name; do
          [[ "$name" =~ $pattern ]] ||
            { echo "orphan sweep refused to remove cgroup $name" >&2; exit 1; }
          find "/sys/fs/cgroup/$name" -depth -type d -exec rmdir {} + 2>/dev/null ||
            true
          if [[ -d "/sys/fs/cgroup/$name" ]]; then
            echo "kept $name"
          else
            echo "removed $name"
          fi
        done
      ' scenario-orphan-sweep "$cgroup_pattern" "${candidate_parents[@]}"
  )" || fail "could not sweep ${#candidate_parents[@]} candidate cgroup parents"
  [[ -n "$cgroup_verdicts" ]] ||
    fail "cgroup parent sweep reported no verdict for ${#candidate_parents[@]} candidates"
  while IFS=' ' read -r verdict parent; do
    case "$verdict" in
      removed) removed_parents+=("$parent") ;;
      kept) kept_parents+=("$parent is still in use") ;;
      *) fail "unrecognized cgroup parent sweep verdict: $verdict $parent" ;;
    esac
  done <<<"$cgroup_verdicts"
fi

if (( ${#kept_bridges[@]} > 0 )); then
  report_kept bridge "${kept_bridges[@]}"
fi
if (( ${#kept_parents[@]} > 0 )); then
  report_kept "cgroup parent" "${kept_parents[@]}"
fi
if (( ${#removed_interfaces[@]} > 0 )); then
  report removed interfaces "${removed_interfaces[@]}"
fi
if (( ${#removed_parents[@]} > 0 )); then
  report removed "cgroup parents" "${removed_parents[@]}"
fi
if (( ${#removed_interfaces[@]} == 0 && ${#removed_parents[@]} == 0 )); then
  echo "SecondBox scenario orphan sweep removed no interfaces or cgroup parents"
fi

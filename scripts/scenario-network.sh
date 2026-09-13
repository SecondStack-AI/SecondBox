#!/usr/bin/env bash

# Linux suite processes hold these advisory locks through teardown. The lock
# directory is shared by checkouts using the same explicit workspace root.
# Never unlink lock files: doing so would let two processes lock different
# inodes for the same /24. flock releases reservations even after a crash.
scenario_reserve_network() {
  local directory="$1" index="$2" result_variable="$3" descriptor status
  mkdir -p "$directory" || { echo "SecondBox scenario cannot create network lock directory" >&2; exit 1; }
  exec {descriptor}>"$directory/$index.lock" || { echo "SecondBox scenario cannot open network lock" >&2; exit 1; }
  if flock --nonblock "$descriptor"; then
    printf -v "$result_variable" '%s' "$descriptor"
    return 0
  else
    status=$?
    exec {descriptor}>&-
    if [[ "$status" -ne 1 ]]; then
      echo "SecondBox scenario network reservation failed: flock status $status" >&2
      exit "$status"
    fi
    return 1
  fi
}

# Reserve a primary/relocation pair on the shared host. Profiles 0/1 remain
# available to manually configured runners; scenario pairs use 2..15. Hold the
# locks through teardown, and reject profiles declared even by stopped runners.
scenario_reserve_gvisor_profiles() {
  local directory="$1" primary occupied containers snapshot current remaining container
  containers="$(docker ps -aq)" || return
  occupied=''
  while [[ -n "$containers" ]]; do
    if snapshot="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' $containers 2>&1)"; then
      occupied="$(sed -n 's/^SECONDBOX_GVISOR_NETWORK_PROFILE=//p' <<<"$snapshot")"
      break
    fi
    # Concurrent suite teardown can remove a container between ps and inspect.
    # Retry only after confirming disappearance, with a strictly shrinking
    # inventory. Preserve failures for containers that still exist (and daemon
    # failures); do not misreport a transient helper as an occupied profile.
    current="$(docker ps -aq)" || return
    remaining=''
    for container in $containers; do
      if grep -qxF "$container" <<<"$current"; then remaining+="$container"$'\n'; fi
    done
    remaining="${remaining%$'\n'}"
    if [[ "$remaining" == "$containers" ]]; then
      printf '%s\n' "$snapshot" >&2
      return 1
    fi
    containers="$remaining"
  done
  for ((primary=2; primary<16; primary+=2)); do
    if grep -qxE "$primary|$((primary+1))" <<<"$occupied"; then continue; fi
    if scenario_reserve_network "$directory" "$primary" scenario_gvisor_profile_lock; then
      export SECONDBOX_SCENARIO_GVISOR_NETWORK_PROFILE="$primary"
      export SECONDBOX_SCENARIO_GVISOR_RELOCATION_NETWORK_PROFILE="$((primary+1))"
      return 0
    fi
  done
  echo 'SecondBox scenario has no free gVisor network profile pair (2..15)' >&2
  return 1
}

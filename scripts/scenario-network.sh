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

#!/usr/bin/env bash
# Read `go test -list` output on stdin and emit an anchored top-level -run
# expression. Shards are one-based, sorted by name, and assigned round-robin.
set -euo pipefail
shard="${1:-}"
if [[ ! "$shard" =~ ^([1-9][0-9]{0,5})/([1-9][0-9]{0,5})$ ]]; then
  echo 'SecondBox scenario shard must be i/N with 1 <= i <= N (at most 999999)' >&2
  exit 1
fi
index="${BASH_REMATCH[1]}"
total="${BASH_REMATCH[2]}"
if (( index > total )); then
  echo 'SecondBox scenario shard index exceeds shard count' >&2
  exit 1
fi
LC_ALL=C sort -u | awk -v shard_index="$index" -v total="$total" '
  /^Test[[:alnum:]_]+$/ {
    if (count++ % total == shard_index - 1) selected = selected separator $0
    if (selected != "") separator = "|"
  }
  END {
    if (count < total) {
      print "SecondBox scenario shard count exceeds top-level test count" > "/dev/stderr"
      exit 1
    }
    print "^(" selected ")$"
  }
'

#!/usr/bin/env bash
set -euo pipefail
count="${1:?shard count}" directory="${2:?shard directory}" output="${3:?output file}"
fail() { echo "SecondBox scenario evidence merge: $*" >&2; exit 1; }
[[ "$count" =~ ^[1-9][0-9]*$ ]] && ((count <= 64)) || fail 'count must be 1..64'
rm -f -- "$output"
files=() results=()
for ((i=1; i<=count; i++)); do
  file="$directory/scenario-shard-$i-evidence.json"
  [[ -f "$file" && ! -L "$file" && -f "$file.results.json" && ! -L "$file.results.json" ]] || fail "missing shard $i"
  jq -e --arg shard "$i/$count" '.shard == $shard and (.tier == "release" or .tier == "nightly") and (.tests | length > 0)' "$file.results.json" >/dev/null || fail "invalid shard $i results"
  files+=("$file") results+=("$file.results.json")
done
jq -se '
  (map(.tier) | unique | length == 1) and
  ([.[].tests[].test] | length == (unique | length)) and
  all(.[].tests[]; .result == "PASS" or .result == "SKIP")
' "${results[@]}" >/dev/null || fail 'inconsistent tiers or overlapping test results'
for ((i=0; i<count; i++)); do
  jq -e --slurpfile results "${results[$i]}" '.passCount == ([$results[0].tests[] | select(.result == "PASS")] | length)' "${files[$i]}" >/dev/null || fail 'pass count differs from test results'
done
commit="$(git rev-parse HEAD)"
[[ -z "$(git status --porcelain --untracked-files=all)" ]] || fail 'merge requires a clean checkout'
temporary="$output.tmp.$$"
trap 'rm -f -- "$temporary"' EXIT
jq -se --arg commit "$commit" '
  if all(.[]; .schemaVersion == "secondbox.release/qualification-evidence/v2" and .sourceCommit == $commit and .repositoryDirty == false and (.passCount > 0) and (.wallClockSeconds >= 0)) and
    (map({schemaVersion,sourceCommit,repositoryDirty,suite,backend,host}) | unique | length == 1)
  then .[0] + {skipped:([.[] | (.skipped // [])[]] | unique),passCount:(map(.passCount)|add),wallClockSeconds:(map(.wallClockSeconds)|max),qualifiedAt:(map(.qualifiedAt)|max)}
  else error("inconsistent shard evidence or source identity") end
' "${files[@]}" >"$temporary" || fail 'shards do not qualify the same clean commit and host'
mv -- "$temporary" "$output"
echo "SecondBox merged $count shards: $output"

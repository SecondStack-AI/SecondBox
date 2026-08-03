#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="${SECONDBOX_RELAY_RETENTION_OUTPUT:-$repo_root/.tmp/relay-retention/result.json}"

if [[ "$output" != /* ]]; then
  echo "SecondBox relay-retention output must be an absolute path: $output" >&2
  exit 1
fi

mkdir -p "$(dirname "$output")"
export SECONDBOX_RELAY_RETENTION_OUTPUT="$output"
export SECONDBOX_SCENARIO_MODE=suite
export SECONDBOX_SCENARIO_TEST_PATTERN='^TestScenarioRelayRetentionMeasurement$'

"$repo_root/scripts/test-scenario.sh"
echo "SecondBox relay-retention result: $output"

package deployment_test

import (
	"os/exec"
	"testing"
)

func TestScenarioSchedulerSharesBudgetAndWaitsForReadiness(t *testing.T) {
	for _, budget := range []string{"1", "4"} {
		t.Run(budget, func(t *testing.T) {
			command := exec.Command("bash", "-euc", `
source <(sed -n '/^stack_stage() (/,/^)/p' ../../scripts/qualify.sh)
source <(sed -n '/^stage() {/,/^}/p' ../../scripts/qualify.sh)
directory="$1" QUALIFY_MAX_STACKS="$2" QUALIFY_GATES_FIRST=0 only=all caller_umask=0022
export directory
cat >"$directory/stack" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
exec 8>"$directory/events.lock"
flock 8
# No second startup may begin while the previous stack is unready.
[[ ! -e "$directory/starting" ]]
touch "$directory/starting" "$directory/active-$1"
active=("$directory"/active-*)
count=${#active[@]}
[[ -f "$directory/test.status" ]] || count=$((count+1))
((count <= $2))
echo "start $1 $count" >>"$directory/events"
flock -u 8
sleep 0.1
flock 8
rm "$directory/starting"
echo "ready $1" >>"$directory/events"
touch "$SECONDBOX_SCENARIO_READY_FILE"
flock -u 8
sleep 0.3
flock 8
rm "$directory/active-$1"
echo "end $1" >>"$directory/events"
SH
chmod +x "$directory/stack"
(sleep 0.3; touch "$directory/test.status") & gate=$!
jobs=()
for backend in firecracker gvisor; do
 for shard in 1 2 3 4; do
  stack_stage "$backend-$shard" "$directory/stack" "$backend-$shard" "$QUALIFY_MAX_STACKS" & jobs+=("$!")
 done
done
wait "$gate"
for job in "${jobs[@]}"; do wait "$job"; done
for backend in firecracker gvisor; do
 for shard in 1 2 3 4; do
  read -r name code elapsed <"$directory/$backend-$shard.status"
  [[ "$code" == 0 ]] || { cat "$directory/$backend-$shard.log"; exit 1; }
 done
done
[[ "$(grep -c '^end ' "$directory/events")" == 8 ]]
# A failed startup must release both its slot and the admission barrier.
stack_stage failed bash -c 'exit 23'
read -r name code elapsed <"$directory/failed.status"
[[ "$code" == 23 ]]
stack_stage survivor "$directory/stack" survivor "$QUALIFY_MAX_STACKS"
read -r name code elapsed <"$directory/survivor.status"
[[ "$code" == 0 ]]
`, "scheduler", t.TempDir(), budget)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("scheduler: %v\n%s", err, output)
			}
		})
	}
}

func TestScenarioReadinessPublishesOnlyAfterHTTPResponse(t *testing.T) {
	command := exec.Command("bash", "-euc", `
export SECONDBOX_SCENARIO_READY_FILE="$1/ready" SECONDBOX_LIVE_BASE_URL=http://127.0.0.1:12345
attempt=0
curl() {
 [[ "$*" == '--fail --silent --show-error --max-time 2 http://127.0.0.1:12345/readyz' ]]
 [[ ! -e "$SECONDBOX_SCENARIO_READY_FILE" ]]
 attempt=$((attempt+1))
 ((attempt == 2))
}
fail() { echo "$*"; exit 1; }
source <(sed -n '/^scenario_ready_deadline=/,/^fi/p' ../../scripts/test-scenario.sh)
[[ "$attempt" == 2 && -f "$SECONDBOX_SCENARIO_READY_FILE" ]]
`, "readiness", t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("HTTP readiness: %v\n%s", err, output)
	}
}

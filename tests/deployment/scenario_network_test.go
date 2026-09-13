package deployment_test

import (
	"os/exec"
	"runtime"
	"testing"
)

func TestScenarioNetworkReservationSurvivesConcurrentSelection(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux scenario network reservation uses flock")
	}
	command := exec.Command("bash", "-euc", `
source ../../scripts/scenario-network.sh
scenario_reserve_network "$1" 100 first
# A separate process cannot claim the guest subnet for its Compose subnet.
bash -euc 'source ../../scripts/scenario-network.sh; ! scenario_reserve_network "$1" 100 other; scenario_reserve_network "$1" 101 other' child "$1"
exec {first}>&-
# Releasing the owner permits reuse, without removing the lock file.
scenario_reserve_network "$1" 100 next
`, "test", t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("network reservations: %v\n%s", err, output)
	}
}

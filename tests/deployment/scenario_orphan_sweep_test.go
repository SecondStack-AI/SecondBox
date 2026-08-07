package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The scenario suite names one bridge, one TAP prefix, and one jailer cgroup
// parent per run from its own PID. A run killed with SIGKILL never reaches its
// EXIT trap, so the suite sweeps orphans from earlier runs. The sweep must
// reclaim only names in the suite's own scheme that nothing is still using: the
// runner devnet bridge, Docker bridges, a bridge a running container declares, a
// bridge carrying a foreign interface, and a cgroup with live members must all
// survive it. A dead run's own TAP belongs to that run and goes with its bridge.
func TestScenarioOrphanSweepReclaimsOnlyDeadSuiteResources(t *testing.T) {
	sweep := newOrphanSweepHarness(t)
	sweep.bridges = []string{
		"1: docker0: <BROADCAST,MULTICAST,UP> mtu 1500",
		"2: br-1365c2e0716c: <BROADCAST,MULTICAST,UP> mtu 1500",
		"3: sbxdev0: <BROADCAST,MULTICAST,UP> mtu 1500",
		"4: sbq013: <BROADCAST,MULTICAST,UP> mtu 1500",
		"5: sbxq10: <BROADCAST,MULTICAST,UP> mtu 1500",
		"6: sbxq20: <BROADCAST,MULTICAST,UP> mtu 1500",
		"7: sbxq30: <BROADCAST,MULTICAST,UP> mtu 1500",
		"8: sbxq34680: <BROADCAST,MULTICAST,UP> mtu 1500",
	}
	// A run with PID 1834680 names its bridge from pid%100000 and its TAPs from
	// pid%1000, so sq680* belongs to sbxq34680 and goes with it. sbxq30 carries
	// an interface from outside the scheme and keeps the whole bridge alive.
	sweep.enslaved = map[string][]string{
		"sbxq30":    {"vethfeed01"},
		"sbxq34680": {"sq68007723a3a58", "sq680b1c2d3e4f"},
	}
	sweep.runningContainerEnvironment = []string{
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME=sbxdev0",
		"SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT=secondbox-dev-runner",
		"SECONDBOX_RUNNER_SANDBOX_BRIDGE_NAME=sbxq20",
		"SECONDBOX_RUNNER_FIRECRACKER_CGROUP_PARENT=secondbox-scenario-20",
	}
	sweep.cgroups = []string{
		"secondbox-scenario-10/firecracker/instance-a",
		"secondbox-scenario-20",
		"secondbox-scenario-30/firecracker/instance-b",
		"secondbox-runner",
		"secondbox-dev-runner",
	}
	// The privileged container removes what the kernel lets it remove and
	// reports one verdict per candidate.
	sweep.cgroupVerdicts = "removed secondbox-scenario-10\nkept secondbox-scenario-30\n"

	output := sweep.run(t)

	candidates := sweep.dockerLog(t)
	for _, required := range []string{
		"scenario-orphan-sweep ^(sbxq[0-9]+|sq[0-9a-f]+)$ sbxq10 sq68007723a3a58 sq680b1c2d3e4f sbxq34680",
		"scenario-orphan-sweep ^secondbox-scenario-[0-9]+$ secondbox-scenario-10 secondbox-scenario-30",
	} {
		if !strings.Contains(candidates, required) {
			t.Errorf("orphan sweep did not offer %q for removal:\n%s", required, candidates)
		}
	}
	for _, forbidden := range []string{
		"sbxq20", "sbxq30", "vethfeed01", "sbxdev0", "sbq013", "docker0", "br-1365c2e0716c",
		"secondbox-scenario-20", "secondbox-runner", "secondbox-dev-runner",
	} {
		if strings.Contains(candidates, " "+forbidden) {
			t.Errorf("orphan sweep offered live or out-of-scheme resource %q:\n%s", forbidden, candidates)
		}
	}
	for _, required := range []string{
		"removed interfaces (4): sbxq10 sq68007723a3a58 sq680b1c2d3e4f sbxq34680",
		"removed cgroup parents (1): secondbox-scenario-10",
		"kept bridge: sbxq20 is declared by a running container",
		"kept bridge: sbxq30 has an enslaved interface outside its own scheme: vethfeed01",
		"kept cgroup parent: secondbox-scenario-20 is declared by a running container",
		"kept cgroup parent: secondbox-scenario-30 is still in use",
	} {
		if !strings.Contains(output, required) {
			t.Errorf("orphan sweep did not report %q:\n%s", required, output)
		}
	}
}

func TestScenarioOrphanSweepReportsAnAlreadyCleanHostAndRemovesNothing(t *testing.T) {
	sweep := newOrphanSweepHarness(t)
	sweep.bridges = []string{
		"1: docker0: <BROADCAST,MULTICAST,UP> mtu 1500",
		"2: sbxdev0: <BROADCAST,MULTICAST,UP> mtu 1500",
	}
	sweep.cgroups = []string{"secondbox-runner"}

	output := sweep.run(t)

	if !strings.Contains(output, "removed no interfaces or cgroup parents") {
		t.Fatalf("orphan sweep did not report a clean host:\n%s", output)
	}
	if removals := sweep.dockerLog(t); strings.Contains(removals, "--privileged") {
		t.Fatalf("orphan sweep started a privileged container with nothing to remove:\n%s", removals)
	}
}

// The suite's own teardown must release the bridge and the cgroup parent it
// named, and must prove it did. The Runner container restarts unless it is
// stopped and reapplies host networking on every start, so a host-network
// removal that runs before the stop can be undone by a restart. The Runner also
// writes its host-network state as root into a 0700 directory, so an
// unprivileged readability test of that state can never gate the removal.
func TestScenarioHarnessReleasesAndProvesItsPerRunHostResources(t *testing.T) {
	harness := readRepositoryFile(t, "scripts/test-scenario.sh")
	for _, required := range []string{
		"scripts/scenario-sweep-host-orphans.sh",
		"sweep_host_orphans",
		`ip link show "$SECONDBOX_SCENARIO_BRIDGE_NAME"`,
		`-d "/sys/fs/cgroup/$SECONDBOX_SCENARIO_CGROUP_PARENT"`,
		"SecondBox scenario host-network cleanup left the bridge behind",
		"SecondBox scenario cgroup parent survived cleanup",
	} {
		if !strings.Contains(harness, required) {
			t.Errorf("scenario harness does not release its per-run host resources through %q", required)
		}
	}
	if strings.Contains(harness, `-f "$state_dir/network/host-network.state"`) {
		t.Error("scenario teardown gates the host-network removal on state only root can read")
	}
	stop := strings.Index(harness, "compose stop secondbox-runner")
	remove := strings.Index(harness, "if ! remove_host_network; then")
	proof := strings.Index(harness, `if ip link show "$SECONDBOX_SCENARIO_BRIDGE_NAME"`)
	if stop < 0 || remove < 0 || proof < 0 {
		t.Fatalf(
			"scenario teardown lost the Runner stop (%d), the host-network removal (%d), or its proof (%d)",
			stop, remove, proof,
		)
	}
	if stop > remove {
		t.Error("scenario teardown removes host networking before the Runner can no longer restart and reapply it")
	}
	if remove > proof {
		t.Error("scenario teardown proves the bridge is gone before it removes host networking")
	}
}

type orphanSweepHarness struct {
	binDirectory                string
	cgroupRoot                  string
	bridgeTable                 string
	dockerLogPath               string
	containerEnvironmentPath    string
	cgroupVerdictPath           string
	enslavedPath                string
	bridges                     []string
	enslaved                    map[string][]string
	runningContainerEnvironment []string
	cgroups                     []string
	cgroupVerdicts              string
}

func newOrphanSweepHarness(t *testing.T) *orphanSweepHarness {
	t.Helper()
	directory := t.TempDir()
	harness := &orphanSweepHarness{
		binDirectory:             filepath.Join(directory, "bin"),
		cgroupRoot:               filepath.Join(directory, "cgroup"),
		bridgeTable:              filepath.Join(directory, "bridges"),
		dockerLogPath:            filepath.Join(directory, "docker.log"),
		containerEnvironmentPath: filepath.Join(directory, "container-environment"),
		cgroupVerdictPath:        filepath.Join(directory, "cgroup-verdicts"),
		enslavedPath:             filepath.Join(directory, "enslaved"),
	}
	for _, path := range []string{harness.binDirectory, harness.cgroupRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeSweepExecutable(t, filepath.Join(harness.binDirectory, "ip"), `#!/bin/sh
set -eu
case "$1 $2 $3 $4" in
  "-o link show type") cat "$FAKE_IP_BRIDGES" ;;
  "-o link show master")
    index=9
    for member in $(sed -n "s/^$5 //p" "$FAKE_IP_ENSLAVED"); do
      echo "$index: $member: <BROADCAST,MULTICAST,UP> mtu 1500 master $5"
      index=$((index + 1))
    done
    ;;
esac
exit 0
`)
	writeSweepExecutable(t, filepath.Join(harness.binDirectory, "docker"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case "${1:-}" in
  ps) printf 'container-a\ncontainer-b\n' ;;
  inspect) cat "$FAKE_DOCKER_CONTAINER_ENVIRONMENT" ;;
  run)
    case "$*" in
      *secondbox-scenario*) cat "$FAKE_DOCKER_CGROUP_VERDICTS" ;;
    esac
    ;;
esac
exit 0
`)
	return harness
}

func (harness *orphanSweepHarness) run(t *testing.T) string {
	t.Helper()
	if err := os.WriteFile(
		harness.bridgeTable, []byte(strings.Join(harness.bridges, "\n")+"\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		harness.containerEnvironmentPath,
		[]byte(strings.Join(harness.runningContainerEnvironment, "\n")+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	enslaved := ""
	for bridge, members := range harness.enslaved {
		enslaved += bridge + " " + strings.Join(members, " ") + "\n"
	}
	if err := os.WriteFile(harness.enslavedPath, []byte(enslaved), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, relative := range harness.cgroups {
		if err := os.MkdirAll(
			filepath.Join(harness.cgroupRoot, filepath.FromSlash(relative)), 0o700,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		harness.cgroupVerdictPath, []byte(harness.cgroupVerdicts), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness.dockerLogPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(
		"bash",
		filepath.Join(repositoryRootForDeploymentPolicy(t), "scripts", "scenario-sweep-host-orphans.sh"),
	)
	command.Env = []string{
		"PATH=" + harness.binDirectory + ":" + os.Getenv("PATH"),
		"FAKE_IP_BRIDGES=" + harness.bridgeTable,
		"FAKE_IP_ENSLAVED=" + harness.enslavedPath,
		"FAKE_DOCKER_LOG=" + harness.dockerLogPath,
		"FAKE_DOCKER_CONTAINER_ENVIRONMENT=" + harness.containerEnvironmentPath,
		"FAKE_DOCKER_CGROUP_VERDICTS=" + harness.cgroupVerdictPath,
		"SECONDBOX_SCENARIO_SWEEP_IMAGE=scenario-runner-image",
		"SECONDBOX_SCENARIO_SWEEP_CGROUP_ROOT=" + harness.cgroupRoot,
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("orphan sweep: %v\n%s", err, output)
	}
	return string(output)
}

func (harness *orphanSweepHarness) dockerLog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(harness.dockerLogPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeSweepExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

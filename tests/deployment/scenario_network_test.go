package deployment_test

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestGVisorScenarioProfilePairsAvoidOtherRunnersAndConcurrentSuites(t *testing.T) {
	command := exec.Command("bash", "-euc", `
source ../../scripts/scenario-network.sh
docker() {
  if [[ "$1" == ps ]]; then echo deployed-runner
  else echo SECONDBOX_GVISOR_NETWORK_PROFILE=3; fi
}
scenario_reserve_gvisor_profiles "$1"
[[ "$SECONDBOX_SCENARIO_GVISOR_NETWORK_PROFILE" == 4 && "$SECONDBOX_SCENARIO_GVISOR_RELOCATION_NETWORK_PROFILE" == 5 ]]
export -f docker
bash -euc 'source ../../scripts/scenario-network.sh; scenario_reserve_gvisor_profiles "$1"; [[ "$SECONDBOX_SCENARIO_GVISOR_NETWORK_PROFILE" == 6 ]]' child "$1"
exec {scenario_gvisor_profile_lock}>&-
scenario_reserve_gvisor_profiles "$1"
[[ "$SECONDBOX_SCENARIO_GVISOR_NETWORK_PROFILE" == 4 ]]
`, "profiles", t.TempDir())
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("profile reservations: %v\n%s", err, output)
	}
}

func TestGVisorHostFirewallOwnsOnlyMarkedReservedInterfaces(t *testing.T) {
	script, err := filepath.Abs("../../scripts/scenario-gvisor-host-firewall.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stub := `#!/usr/bin/env bash
if [[ "$*" == *'list ruleset'* ]]; then
 echo '{"nftables":[{"chain":{"family":"ip","table":"filter","name":"INPUT"}},{"rule":{"family":"ip","table":"filter","chain":"INPUT","comment":"secondbox-suite-123","handle":42}},{"rule":{"family":"ip","table":"filter","chain":"INPUT","comment":"another-owner","handle":43}}]}'
else cat; fi
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"apply", "remove"} {
		command := exec.Command("bash", script, action, "image", "secondbox-suite-123", "4", "5")
		command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", action, err, output)
		}
		expected := "delete rule ip filter INPUT handle 42\n"
		if action == "apply" {
			expected = "insert rule ip filter INPUT iifname \"gvh4-*\" ct mark 0x53425801 counter accept comment \"secondbox-suite-123\"\ninsert rule ip filter INPUT iifname \"gvh5-*\" ct mark 0x53425801 counter accept comment \"secondbox-suite-123\"\n"
		}
		if string(output) != expected {
			t.Fatalf("%s rules = %s", action, output)
		}
	}
}

func TestDirectScenarioHostFirewallScopesAdmissionAndCleanup(t *testing.T) {
	script, err := filepath.Abs("../../scripts/scenario-direct-host-firewall.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stub := `#!/usr/bin/env bash
if [[ "$*" == *'list ruleset'* ]]; then
 echo "${TEST_RULESET}"
else cat; fi
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	ruleset := `{"nftables":[{"chain":{"family":"ip","table":"filter","name":"INPUT"}},{"rule":{"family":"ip","table":"filter","chain":"INPUT","comment":"secondbox-suite-123-direct-data-plane","handle":42}},{"rule":{"family":"ip","table":"filter","chain":"INPUT","comment":"secondbox-suite-456-direct-data-plane","handle":43}},{"rule":{"family":"ip","table":"filter","chain":"INPUT","comment":"secondbox-suite-123","handle":44}}]}`
	for _, test := range []struct {
		name, action, owner, subnet, gateway, port, ruleset, want string
		invalid                                                   bool
	}{
		{name: "apply", action: "apply", want: "insert rule ip filter INPUT ip saddr 198.18.33.0/24 ip daddr 198.18.33.1 tcp dport { 56543, 56544 } counter accept comment \"secondbox-suite-123-direct-data-plane\"\n"},
		{name: "remove only owned rule", action: "remove", want: "delete rule ip filter INPUT handle 42\n"},
		{name: "no host firewall", action: "apply", ruleset: `{"nftables":[]}`},
		{name: "foreign owner", action: "apply", owner: "production", invalid: true},
		{name: "foreign subnet", action: "apply", subnet: "10.0.0.0/24", invalid: true},
		{name: "foreign gateway", action: "apply", gateway: "198.18.34.1", invalid: true},
		{name: "invalid port", action: "apply", port: "65536", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.owner == "" {
				test.owner = "secondbox-suite-123"
			}
			if test.subnet == "" {
				test.subnet = "198.18.33.0/24"
			}
			if test.gateway == "" {
				test.gateway = "198.18.33.1"
			}
			if test.port == "" {
				test.port = "56543"
			}
			if test.ruleset == "" {
				test.ruleset = ruleset
			}
			command := exec.Command("bash", script, test.action, "image", test.owner, test.subnet, test.gateway, test.port, "56544")
			command.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_RULESET="+test.ruleset)
			output, err := command.CombinedOutput()
			if (err != nil) != test.invalid || string(output) != test.want {
				t.Fatalf("firewall result = %q, %v; want %q, invalid=%v", output, err, test.want, test.invalid)
			}
		})
	}
}

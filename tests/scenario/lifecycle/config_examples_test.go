package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exampleGuestCIDR = "198.18.0.0/24"

func examplePath(name string) string {
	return filepath.Join("..", "..", "..", "scripts", name)
}

func loadExample(t *testing.T, name string) lifecycleConfig {
	t.Helper()
	absolute, err := filepath.Abs(examplePath(name))
	if err != nil {
		t.Fatal(err)
	}
	config, err := readLifecycleConfig(absolute)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return config
}

func TestEveryShippedExampleConfigLoads(t *testing.T) {
	for _, name := range []string{
		"lifecycle-config.example.json",
		"capacity-config.example.json",
		"capacity-gate-config.example.json",
	} {
		t.Run(name, func(t *testing.T) {
			config := loadExample(t, name)
			if config.Version != 3 {
				t.Fatalf("version = %d, want 3", config.Version)
			}
			if config.HostRails == nil || config.Gate == nil {
				t.Fatal("rails and gate must both be present and explicit")
			}
		})
	}
}

func TestConfigRejectsUnknownFieldsAndSupersededVersions(t *testing.T) {
	base := map[string]any{}
	raw, err := os.ReadFile(examplePath("lifecycle-config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatal(err)
	}

	t.Run("unknown field", func(t *testing.T) {
		mutated := map[string]any{}
		for key, value := range base {
			mutated[key] = value
		}
		mutated["overdrive"] = true
		if err := writeAndRead(t, mutated); err == nil ||
			!strings.Contains(err.Error(), "overdrive") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("superseded version", func(t *testing.T) {
		mutated := map[string]any{}
		for key, value := range base {
			mutated[key] = value
		}
		mutated["version"] = 2
		if err := writeAndRead(t, mutated); err == nil ||
			!strings.Contains(err.Error(), "version must be 3") {
			t.Fatalf("error = %v", err)
		}
	})
}

func writeAndRead(t *testing.T, config map[string]any) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readLifecycleConfig(path)
	return err
}

func ladderMaximum(config lifecycleConfig) int {
	highest := 0
	for _, pattern := range config.Patterns {
		if pattern.Count > highest {
			highest = pattern.Count
		}
	}
	return highest
}

// Discovery raises every configured limit above the ladder so the machine is
// what binds. Its expected signal is a distress knee, not a clean refusal, which
// is exactly why it observes rather than enforces.
func TestDiscoveryConfigLetsTheHostBindRatherThanTheConfiguration(t *testing.T) {
	config := loadExample(t, "capacity-config.example.json")
	ladder := ladderMaximum(config)
	binding := config.configuredBinding(exampleGuestCIDR)
	if binding.Capacity <= ladder {
		t.Fatalf(
			"configured ceiling %d (%s) is inside the ladder, which tops out at %d: "+
				"the configuration would bind before the host",
			binding.Capacity, binding.Name, ladder,
		)
	}
	if config.Gate.Mode != gateObserve {
		t.Fatalf("discovery gate mode = %q, want %q", config.Gate.Mode, gateObserve)
	}
	if !config.HostRails.Enabled {
		t.Fatal("a host-bound run must have rails enabled")
	}
}

// The gate keeps one configured limit inside the ladder so the clean refusal
// path is genuinely exercised. Without that, nothing refuses and the checks
// assert against a case the run can never produce.
func TestGateConfigKeepsAConfiguredLimitInsideTheLadder(t *testing.T) {
	config := loadExample(t, "capacity-gate-config.example.json")
	ladder := ladderMaximum(config)
	binding := config.configuredBinding(exampleGuestCIDR)
	if binding.Capacity >= ladder {
		t.Fatalf(
			"configured ceiling %d (%s) is at or above the ladder maximum %d, "+
				"so no rung would ever be refused",
			binding.Capacity, binding.Name, ladder,
		)
	}
	if config.Gate.Mode != gateEnforce {
		t.Fatalf("gate mode = %q, want %q", config.Gate.Mode, gateEnforce)
	}
	if config.Gate.DeclaredCeiling != binding.Capacity {
		t.Fatalf(
			"declared ceiling %d does not match the configured binding %d (%s)",
			config.Gate.DeclaredCeiling, binding.Capacity, binding.Name,
		)
	}
	// The ladder has to straddle the ceiling, or one side of the gate is untested.
	below, above := 0, 0
	for _, pattern := range config.Patterns {
		if pattern.Count <= config.Gate.DeclaredCeiling {
			below++
		} else {
			above++
		}
	}
	if below == 0 || above == 0 {
		t.Fatalf("ladder does not straddle the ceiling: %d below, %d above", below, above)
	}
}

// A capacity ladder whose rungs exceed maximumInFlight measures the driver's own
// shedding cap. Both shipped capacity configs must clear it.
func TestCapacityConfigsOfferEveryRungToTheDeployment(t *testing.T) {
	for _, name := range []string{
		"capacity-config.example.json",
		"capacity-gate-config.example.json",
	} {
		t.Run(name, func(t *testing.T) {
			config := loadExample(t, name)
			if ladder := ladderMaximum(config); ladder > config.MaximumInFlight {
				t.Fatalf(
					"largest rung %d exceeds maximumInFlight %d",
					ladder, config.MaximumInFlight,
				)
			}
		})
	}
}

// prepareMeasurementPool pre-creates one Sandbox per arrival, serially, for every
// measurement except create_to_ready. A 128-rung on any other measurement would
// spend the run provisioning.
func TestCapacityConfigsMeasureOnlyCreateToReady(t *testing.T) {
	for _, name := range []string{
		"capacity-config.example.json",
		"capacity-gate-config.example.json",
	} {
		t.Run(name, func(t *testing.T) {
			config := loadExample(t, name)
			if len(config.Measurements) != 1 ||
				config.Measurements[0] != measurementCreateReady {
				t.Fatalf("measurements = %v, want only %s", config.Measurements, measurementCreateReady)
			}
		})
	}
}

// The scenario harness allocates a /24 per run, so no ladder may ask for more
// concurrent Sandboxes than the guest network can address.
func TestCapacityLaddersStayWithinTheGuestAddressSpace(t *testing.T) {
	for _, name := range []string{
		"capacity-config.example.json",
		"capacity-gate-config.example.json",
	} {
		t.Run(name, func(t *testing.T) {
			config := loadExample(t, name)
			if ladder := ladderMaximum(config); ladder > guestIPCapacity(exampleGuestCIDR) {
				t.Fatalf(
					"largest rung %d exceeds the %d guest addresses a /24 provides",
					ladder, guestIPCapacity(exampleGuestCIDR),
				)
			}
		})
	}
}

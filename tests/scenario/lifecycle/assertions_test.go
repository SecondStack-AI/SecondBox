package main

import (
	"strings"
	"testing"
)

func gateAt(ceiling int) gateConfig {
	return gateConfig{Mode: gateEnforce, DeclaredCeiling: ceiling}
}

func checkNames(violations []gateViolation) []string {
	names := make([]string, 0, len(violations))
	for _, violation := range violations {
		names = append(names, violation.Check)
	}
	return names
}

func TestGateAcceptsALadderThatShedsCleanly(t *testing.T) {
	violations := evaluateGate(gateAt(32), []cellResult{
		rung(8), rung(16), rung(32),
		rung(64, withRefusals(map[string]int64{"quota_exceeded": 32})),
	})
	if len(violations) != 0 {
		t.Fatalf("clean ladder produced violations: %v", violations)
	}
}

func TestGateRejectsRefusalBelowTheDeclaredCeiling(t *testing.T) {
	violations := evaluateGate(gateAt(32), []cellResult{
		rung(8), rung(16, withRefusals(map[string]int64{"quota_exceeded": 1})),
	})
	if len(violations) != 1 || violations[0].Check != "below-ceiling-clean" {
		t.Fatalf("violations = %v", violations)
	}
	if !strings.Contains(violations[0].Message, "quota_exceeded") {
		t.Fatalf("violation does not name the refusal: %s", violations[0].Message)
	}
}

func TestGateRejectsFailureBelowTheDeclaredCeiling(t *testing.T) {
	violations := evaluateGate(gateAt(32), []cellResult{
		rung(16, withFailures(map[string]int64{"startup_failed": 2})),
	})
	if len(violations) != 1 || violations[0].Check != "below-ceiling-clean" {
		t.Fatalf("violations = %v", violations)
	}
}

// Past its ceiling a deployment must decline work, not silently absorb more
// than it declared it could.
func TestGateRejectsSilentAbsorptionAboveTheCeiling(t *testing.T) {
	violations := evaluateGate(gateAt(32), []cellResult{rung(64)})
	if len(violations) != 1 || violations[0].Check != "above-ceiling-sheds" {
		t.Fatalf("violations = %v", violations)
	}
	if !strings.Contains(violations[0].Message, "nothing was refused") {
		t.Fatalf("violation message = %s", violations[0].Message)
	}
}

// Shedding is acceptable past the ceiling; breaking is not.
func TestGateRejectsFailureAboveTheCeiling(t *testing.T) {
	violations := evaluateGate(gateAt(32), []cellResult{
		rung(64,
			withRefusals(map[string]int64{"quota_exceeded": 10}),
			withFailures(map[string]int64{"startup_failed": 5}),
		),
	})
	if len(violations) != 1 || violations[0].Check != "above-ceiling-sheds" {
		t.Fatalf("violations = %v", violations)
	}
	if !strings.Contains(violations[0].Message, "rather than refusals") {
		t.Fatalf("violation message = %s", violations[0].Message)
	}
}

// An arrival the driver shed never reached the deployment, so the cell measured
// maximumInFlight instead of the system under test.
func TestGateRejectsCellsTheDriverShed(t *testing.T) {
	shedCell := rung(64, withRefusals(map[string]int64{"quota_exceeded": 8}))
	shedCell.ShedArrivals = 40
	violations := evaluateGate(gateAt(32), []cellResult{shedCell})
	if len(violations) != 1 || violations[0].Check != "measured-the-deployment" {
		t.Fatalf("violations = %v", violations)
	}
	if !strings.Contains(violations[0].Message, "maximumInFlight") {
		t.Fatalf("violation message = %s", violations[0].Message)
	}
}

func TestGateRejectsPeakOutstandingBeyondWhatWasOffered(t *testing.T) {
	cell := rung(16)
	cell.PeakOutstandingArrivals = 20
	violations := evaluateGate(gateAt(32), []cellResult{cell})
	if len(violations) != 1 || violations[0].Check != "measured-the-deployment" {
		t.Fatalf("violations = %v", violations)
	}
}

func TestGateReportsEveryViolationRatherThanTheFirst(t *testing.T) {
	below := rung(8, withRefusals(map[string]int64{"quota_exceeded": 1}))
	above := rung(64)
	above.ShedArrivals = 4
	violations := evaluateGate(gateAt(32), []cellResult{below, above})
	names := checkNames(violations)
	if len(violations) != 3 {
		t.Fatalf("violations = %v, want 3", names)
	}
	for _, expected := range []string{
		"below-ceiling-clean", "above-ceiling-sheds", "measured-the-deployment",
	} {
		found := false
		for _, name := range names {
			if name == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("violations %v are missing %q", names, expected)
		}
	}
}

func TestGateModeIsRequiredAndExplicit(t *testing.T) {
	t.Run("block required", func(t *testing.T) {
		config := validLifecycleConfig()
		config.Gate = nil
		err := validateLifecycleConfig(config)
		if err == nil || !strings.Contains(err.Error(), "gate block") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("unknown mode rejected", func(t *testing.T) {
		config := validLifecycleConfig()
		config.Gate = &gateConfig{Mode: "maybe"}
		err := validateLifecycleConfig(config)
		if err == nil || !strings.Contains(err.Error(), "gate.mode") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("enforcing requires a ceiling", func(t *testing.T) {
		config := validLifecycleConfig()
		config.Gate = &gateConfig{Mode: gateEnforce}
		err := validateLifecycleConfig(config)
		if err == nil || !strings.Contains(err.Error(), "declaredCeiling") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("observing needs no ceiling", func(t *testing.T) {
		config := validLifecycleConfig()
		config.Gate = &gateConfig{Mode: gateObserve}
		if err := validateLifecycleConfig(config); err != nil {
			t.Fatal(err)
		}
	})
}

// A discovery run declares no ceiling, because finding one is the point. With a
// declared ceiling of zero every rung would otherwise count as overload and be
// reported for failing to refuse, burying the checks that do apply.
func TestGateWithoutADeclaredCeilingSkipsCeilingChecks(t *testing.T) {
	observe := gateConfig{Mode: gateObserve, DeclaredCeiling: 0}
	violations := evaluateGate(observe, []cellResult{rung(8), rung(16), rung(32)})
	if len(violations) != 0 {
		t.Fatalf("a run with no declared ceiling reported %v", checkNames(violations))
	}
}

// The checks that do not depend on a ceiling still apply without one.
func TestGateWithoutACeilingStillCatchesDriverShedding(t *testing.T) {
	shedCell := rung(32)
	shedCell.ShedArrivals = 4
	violations := evaluateGate(
		gateConfig{Mode: gateObserve, DeclaredCeiling: 0}, []cellResult{shedCell},
	)
	if len(violations) != 1 || violations[0].Check != "measured-the-deployment" {
		t.Fatalf("violations = %v", checkNames(violations))
	}
}

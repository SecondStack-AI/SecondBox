package main

import "testing"

func TestLifecyclePhaseModelCoversBoundedOperations(t *testing.T) {
	required := []lifecyclePhase{phaseCreate, phaseScheduling, phaseRunnerAdmission, phaseReadiness, phaseExecNegotiation, phaseStop, phaseStart, phaseDrain, phaseDelete, phaseSnapshot, phaseRelocation, phaseTerminalAttachment}
	seen := map[lifecyclePhase]bool{}
	for _, phases := range lifecyclePhases {
		for _, phase := range phases {
			seen[phase] = true
		}
	}
	for _, phase := range required {
		if !seen[phase] {
			t.Errorf("lifecycle phase %q is not modeled", phase)
		}
	}
}

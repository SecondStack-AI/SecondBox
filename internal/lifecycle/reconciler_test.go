package lifecycle

import (
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestDesiredStateTransitionsAreExplicit(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		view View
		want Action
	}{
		{"create stopped waits for Workspace evidence", View{Observed: contracts.SandboxStateCreating, Desired: contracts.SandboxDesiredStateStopped}, ActionWait},
		{"already stopped", View{Observed: contracts.SandboxStateStopped, Desired: contracts.SandboxDesiredStateStopped}, ActionWait},
		{"create running waits for Workspace receipt", View{Observed: contracts.SandboxStateCreating, Desired: contracts.SandboxDesiredStateRunning}, ActionWait},
		{"start", View{Observed: contracts.SandboxStateStopped, Desired: contracts.SandboxDesiredStateRunning}, ActionStartInstance},
		{"restart retries missing durable assignment", View{Observed: contracts.SandboxStateStarting, Desired: contracts.SandboxDesiredStateRunning}, ActionStartInstance},
		{"assignment awaits runner ready evidence", View{Observed: contracts.SandboxStateStarting, Desired: contracts.SandboxDesiredStateRunning, HasInstance: true}, ActionWait},
		{"guest ready", View{Observed: contracts.SandboxStateStarting, Desired: contracts.SandboxDesiredStateRunning, HasInstance: true, GuestLiveness: contracts.GuestLivenessReady}, ActionMarkReady},
		{"explicit stop", View{Observed: contracts.SandboxStateReady, Desired: contracts.SandboxDesiredStateStopped}, ActionDrain},
		{"drained", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateStopped, ActiveSessions: 0}, ActionStopInstance},
		{"delete running", View{Observed: contracts.SandboxStateReady, Desired: contracts.SandboxDesiredStateDeleted}, ActionDrain},
		{"delete never materialized", View{Observed: contracts.SandboxStateCreating, Desired: contracts.SandboxDesiredStateDeleted}, ActionDelete},
		{"delete drained", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateDeleted}, ActionStopInstance},
		{"delete starting stops", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateDeleted, HasInstance: true, GuestLiveness: contracts.GuestLivenessStarting}, ActionStopInstance},
		{"stopped compute waits for local generation receipt", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateRunning, GuestLiveness: contracts.GuestLivenessStopped}, ActionWait},
		{"start waits through completed stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateRunning, GuestLiveness: contracts.GuestLivenessStopped, StopEffectState: "runner_succeeded"}, ActionFinishStop},
		{"instance without stopped evidence resumes stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateStopped, HasInstance: true}, ActionStopInstance},
		{"running intent resumes stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateRunning, HasInstance: true}, ActionStopInstance},
		{"delete intent resumes stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateDeleted, HasInstance: true}, ActionStopInstance},
		{"delete commits local generation before deletion", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateDeleted, GuestLiveness: contracts.GuestLivenessStopped, StopEffectState: "runner_succeeded"}, ActionFinishStop},
		{"delete commits local generation after runner loss", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateDeleted, HasInstance: true, GuestLiveness: contracts.GuestLivenessLost, StopEffectState: "runner_succeeded"}, ActionFinishStop},
		{"stop retry exhaustion fails terminally", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateStopped, HasInstance: true, StopEffectState: "runner_failed"}, ActionFail},
		{"guest loss stops", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateStopped, HasInstance: true, GuestLiveness: contracts.GuestLivenessLost}, ActionStopInstance},
		{"no instance still waits for local generation receipt", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateStopped}, ActionWait},
		{"no instance with local generation receipt finishes stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateStopped, StopEffectState: "runner_succeeded"}, ActionFinishStop},
		{"delete stopped", View{Observed: contracts.SandboxStateStopped, Desired: contracts.SandboxDesiredStateDeleted}, ActionDelete},
		{"delete waits for local receipt", View{Observed: contracts.SandboxStateDeleting, Desired: contracts.SandboxDesiredStateDeleted}, ActionDelete},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Decide(test.view, now); got.Action != test.want {
				t.Fatalf("action = %q, want %q (%#v)", got.Action, test.want, got)
			}
		})
	}
}

func TestDrainBarrierExpiresAtProfileBound(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	view := View{
		Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateStopped,
		ActiveSessions: 2, DrainStartedAt: now.Add(-29 * time.Second),
		DrainGrace: 30 * time.Second,
	}
	if got := Decide(view, now); got.Action != ActionWait {
		t.Fatalf("pre-deadline drain action = %#v", got)
	}
	view.DrainStartedAt = now.Add(-30 * time.Second)
	if got := Decide(view, now); got.Action != ActionStopInstance {
		t.Fatalf("expired drain action = %#v", got)
	}
}

func TestIdleMaximumDurationAndGuestLivenessAreIndependent(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	base := View{
		Observed: contracts.SandboxStateReady, Desired: contracts.SandboxDesiredStateRunning,
		ReadyAt: now.Add(-10 * time.Minute), LastUsefulActivityAt: now.Add(-2 * time.Minute),
		IdleTimeout: time.Minute, MaximumDuration: time.Hour,
		GuestLiveness: contracts.GuestLivenessReady,
	}
	if got := Decide(base, now); got.TerminationReason != contracts.TerminationReasonIdleTimeout {
		t.Fatalf("idle decision = %#v", got)
	}
	base.ActiveSessions = 1
	if got := Decide(base, now); got.Action != ActionWait {
		t.Fatalf("active session decision = %#v", got)
	}
	base.ActiveSessions = 0
	base.MaximumDuration = 5 * time.Minute
	if got := Decide(base, now); got.TerminationReason != contracts.TerminationReasonMaximumDuration {
		t.Fatalf("maximum duration decision = %#v", got)
	}
	base.MaximumDuration = time.Hour
	base.LastUsefulActivityAt = now
	base.GuestLiveness = contracts.GuestLivenessLost
	if got := Decide(base, now); got.TerminationReason != contracts.TerminationReasonGuestAgentLost {
		t.Fatalf("guest loss decision = %#v", got)
	}
}

func TestReadySandboxDrainsWithImmutableObservedInstanceReason(t *testing.T) {
	for _, reason := range []string{
		contracts.TerminationReasonGuestShutdown,
		contracts.TerminationReasonResourceExhaustion,
		contracts.TerminationReasonInternalFailure,
	} {
		t.Run(reason, func(t *testing.T) {
			decision := Decide(View{
				Observed:                  contracts.SandboxStateReady,
				Desired:                   contracts.SandboxDesiredStateRunning,
				HasInstance:               true,
				GuestLiveness:             contracts.GuestLivenessStopped,
				InstanceTerminationReason: reason,
			}, time.Now().UTC())
			if decision.Action != ActionDrain || decision.TerminationReason != reason {
				t.Fatalf("decision = %+v, want drain with %q", decision, reason)
			}
		})
	}
}

func TestTerminationReasonsAreStable(t *testing.T) {
	for _, reason := range []string{
		contracts.TerminationReasonRequestedDrain,
		contracts.TerminationReasonRequestedStop,
		contracts.TerminationReasonIdleTimeout,
		contracts.TerminationReasonMaximumDuration,
		contracts.TerminationReasonGuestShutdown,
		contracts.TerminationReasonResourceExhaustion,
		contracts.TerminationReasonGuestAgentLost,
		contracts.TerminationReasonRunnerLost,
		contracts.TerminationReasonStartupFailed,
		contracts.TerminationReasonFenced,
		contracts.TerminationReasonInternalFailure,
	} {
		if !ValidTerminationReason(reason) {
			t.Errorf("termination reason %q is not accepted", reason)
		}
	}
	if ValidTerminationReason("unknown") {
		t.Fatal("unknown termination reason was accepted")
	}
}

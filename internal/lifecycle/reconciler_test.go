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
		{"create stopped", View{Observed: contracts.SandboxStateCreating, Desired: contracts.SandboxDesiredStateStopped}, ActionFinishCreateStopped},
		{"already stopped", View{Observed: contracts.SandboxStateStopped, Desired: contracts.SandboxDesiredStateStopped}, ActionWait},
		{"create running", View{Observed: contracts.SandboxStateCreating, Desired: contracts.SandboxDesiredStateRunning}, ActionMaterialize},
		{"start", View{Observed: contracts.SandboxStateStopped, Desired: contracts.SandboxDesiredStateRunning}, ActionMaterialize},
		{"restart retries missing durable assignment", View{Observed: contracts.SandboxStateStarting, Desired: contracts.SandboxDesiredStateRunning}, ActionMaterialize},
		{"materialization awaits runner ready evidence", View{Observed: contracts.SandboxStateStarting, Desired: contracts.SandboxDesiredStateRunning, MaterializationState: contracts.MaterializationStateReady}, ActionWait},
		{"guest evidence alone is insufficient", View{Observed: contracts.SandboxStateStarting, Desired: contracts.SandboxDesiredStateRunning, GuestLiveness: contracts.GuestLivenessReady}, ActionMaterialize},
		{"guest and materialization ready", View{Observed: contracts.SandboxStateStarting, Desired: contracts.SandboxDesiredStateRunning, MaterializationState: contracts.MaterializationStateReady, GuestLiveness: contracts.GuestLivenessReady}, ActionMarkReady},
		{"explicit stop", View{Observed: contracts.SandboxStateReady, Desired: contracts.SandboxDesiredStateStopped}, ActionDrain},
		{"drained checkpoint", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateStopped, ActiveSessions: 0, HasInstance: true, GuestLiveness: contracts.GuestLivenessReady, CheckpointOnStop: true}, ActionCheckpoint},
		{"explicit checkpoint overrides profile", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateStopped, ActiveSessions: 0, HasInstance: true, GuestLiveness: contracts.GuestLivenessReady, ForceCheckpoint: true}, ActionCheckpoint},
		{"drained no checkpoint", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateStopped, ActiveSessions: 0}, ActionStopInstance},
		{"checkpoint resumes durable effect", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateStopped}, ActionCheckpoint},
		{"checkpoint published needs runner stop", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateStopped, CheckpointState: contracts.ObjectStatePublished, HasInstance: true}, ActionStopInstance},
		{"checkpoint published without instance", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateStopped, CheckpointState: contracts.ObjectStatePublished}, ActionFinishStop},
		{"delete running", View{Observed: contracts.SandboxStateReady, Desired: contracts.SandboxDesiredStateDeleted}, ActionDrain},
		{"delete never materialized", View{Observed: contracts.SandboxStateCreating, Desired: contracts.SandboxDesiredStateDeleted}, ActionDelete},
		{"delete drained", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateDeleted}, ActionStopInstance},
		{"delete starting skips checkpoint", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateDeleted, HasInstance: true, GuestLiveness: contracts.GuestLivenessStarting, CheckpointOnStop: true}, ActionStopInstance},
		{"delete resumes pending checkpoint", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateDeleted}, ActionCheckpoint},
		{"delete bypasses failed checkpoint with instance", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateDeleted, CheckpointState: contracts.ObjectStateIntegrityFailed, HasInstance: true}, ActionStopInstance},
		{"delete bypasses failed checkpoint without instance", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateDeleted, CheckpointState: contracts.ObjectStateIntegrityFailed}, ActionDelete},
		{"delete published checkpoint without instance", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateDeleted, CheckpointState: contracts.ObjectStatePublished}, ActionDelete},
		{"start waits through completed stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateRunning, GuestLiveness: contracts.GuestLivenessStopped}, ActionFinishStop},
		{"start resumes pending checkpoint", View{Observed: contracts.SandboxStateCheckpointing, Desired: contracts.SandboxDesiredStateRunning}, ActionCheckpoint},
		{"instance without stopped evidence resumes stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateStopped, HasInstance: true}, ActionStopInstance},
		{"running intent resumes stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateRunning, HasInstance: true}, ActionStopInstance},
		{"delete intent resumes stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateDeleted, HasInstance: true}, ActionStopInstance},
		{"stop retry exhaustion fails terminally", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateStopped, HasInstance: true, StopEffectState: "runner_failed"}, ActionFail},
		{"guest loss skips checkpoint", View{Observed: contracts.SandboxStateDraining, Desired: contracts.SandboxDesiredStateStopped, HasInstance: true, GuestLiveness: contracts.GuestLivenessLost, CheckpointOnStop: true}, ActionStopInstance},
		{"no instance needs no runner stop", View{Observed: contracts.SandboxStateStopping, Desired: contracts.SandboxDesiredStateStopped}, ActionFinishStop},
		{"delete stopped", View{Observed: contracts.SandboxStateStopped, Desired: contracts.SandboxDesiredStateDeleted}, ActionDelete},
		{"delete final", View{Observed: contracts.SandboxStateDeleting, Desired: contracts.SandboxDesiredStateDeleted}, ActionFinishDelete},
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
	view.CheckpointOnStop = true
	view.HasInstance = true
	view.GuestLiveness = contracts.GuestLivenessReady
	if got := Decide(view, now); got.Action != ActionCheckpoint {
		t.Fatalf("expired checkpoint drain action = %#v", got)
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

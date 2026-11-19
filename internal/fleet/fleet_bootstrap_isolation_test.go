package fleet

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

func TestFleetBootstrapTimeoutDoesNotBlockKnownNodeLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 19, 4, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name          string
		state         string
		wantAction    string
		wantControl   string
		wantTerminate bool
	}{
		{name: "begin evacuation", state: "active", wantAction: "begin_evacuation", wantControl: "evacuate:runner-healthy"},
		{name: "archive stopped workspace", state: "evacuating", wantAction: "evacuate", wantControl: "archive:saved-workspace"},
		{name: "retire evacuated runner", state: "evacuated", wantAction: "retire", wantControl: "retire:runner-healthy"},
		{name: "terminate retired runner", state: "retired", wantAction: "terminate", wantTerminate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cloud := &fleetCloudFixture{nodes: []Node{
				{InstanceID: "i-unregistered", RunnerID: "runner-unregistered", CreatedAt: now.Add(-time.Hour), State: "running"},
				{InstanceID: "i-healthy", RunnerID: "runner-healthy", CreatedAt: now.Add(-time.Hour), State: "running"},
			}}
			control := &fleetControlFixture{observation: contracts.FleetPoolObservation{
				Runners: []contracts.FleetRunner{{ID: "runner-healthy", Connected: true, State: "ready", FleetState: test.state}},
			}}
			if test.state == "evacuating" {
				control.observation.Runners[0].WorkspaceCount = 1
				control.observation.Workspaces = []contracts.FleetWorkspace{{SandboxID: "saved-workspace", RunnerID: "runner-healthy", State: "stopped", DesiredState: "stopped"}}
			}

			result, err := testFleetManager(cloud, control).Reconcile(t.Context(), now)
			if err == nil || !strings.Contains(err.Error(), "i-unregistered") || !strings.Contains(err.Error(), "operator recovery is required") {
				t.Fatalf("bootstrap failure was not reported: result=%+v err=%v", result, err)
			}
			if result.Action != test.wantAction || result.RunnerID != "runner-healthy" || result.ObservedNodes != 2 {
				t.Fatalf("healthy Runner reconciliation was blocked: result=%+v err=%v", result, err)
			}
			if test.wantControl != "" && (len(control.actions) != 1 || control.actions[0] != test.wantControl) {
				t.Fatalf("control actions=%v, want %s", control.actions, test.wantControl)
			}
			if test.wantControl == "" && len(control.actions) != 0 {
				t.Fatalf("unexpected control actions=%v", control.actions)
			}
			if test.wantTerminate {
				if len(cloud.terminated) != 1 || cloud.terminated[0] != "i-healthy" {
					t.Fatalf("termination targets=%v, want only the retired healthy instance", cloud.terminated)
				}
			} else if len(cloud.terminated) != 0 {
				t.Fatalf("unproven instance was terminated: %v", cloud.terminated)
			}
			if len(cloud.desired) != 0 {
				t.Fatalf("lifecycle cleanup changed desired capacity: %v", cloud.desired)
			}
		})
	}
}

func TestFleetBootstrapTimeoutDoesNotSupplyInflightCapacity(t *testing.T) {
	now := time.Date(2026, 11, 19, 4, 0, 0, 0, time.UTC)
	for _, maximumNodes := range []int{1, 2} {
		t.Run(map[int]string{1: "at physical ceiling", 2: "spare physical slot"}[maximumNodes], func(t *testing.T) {
			cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-unregistered", RunnerID: "runner-unregistered", CreatedAt: now.Add(-time.Hour), State: "running"}}}
			control := &fleetControlFixture{observation: contracts.FleetPoolObservation{
				Demand: contracts.FleetResourceDemand{Instances: 1, VCPUCount: 2, MemoryBytes: 4 << 30, WorkspaceBytes: 20 << 30},
			}}
			manager := testFleetManager(cloud, control)
			manager.Config.MaximumNodes = maximumNodes
			result, err := manager.Reconcile(t.Context(), now)
			if err == nil || !strings.Contains(err.Error(), "i-unregistered") {
				t.Fatalf("bootstrap failure was not reported: result=%+v err=%v", result, err)
			}
			if result.ObservedNodes != 1 || result.DesiredNodes != maximumNodes {
				t.Fatalf("failed bootstrap lost its physical slot: result=%+v", result)
			}
			if maximumNodes == 1 {
				if result.Action != "capacity_ceiling" || len(cloud.desired) != 0 {
					t.Fatalf("physical ceiling bypassed: result=%+v desired=%v", result, cloud.desired)
				}
			} else if result.Action != "scale_out" || len(cloud.desired) != 1 || cloud.desired[0] != 2 {
				t.Fatalf("failed bootstrap was counted as usable capacity: result=%+v desired=%v", result, cloud.desired)
			}
			if len(cloud.terminated) != 0 || len(control.actions) != 0 {
				t.Fatalf("unproven instance authority changed: terminated=%v actions=%v", cloud.terminated, control.actions)
			}
		})
	}
}

func TestFleetBootstrapTimeoutDoesNotHideKnownArchiveError(t *testing.T) {
	now := time.Date(2026, 11, 19, 4, 0, 0, 0, time.UTC)
	archiveErr := errors.New("archive upload refused")
	cloud := &fleetCloudFixture{nodes: []Node{
		{InstanceID: "i-unregistered", RunnerID: "runner-unregistered", CreatedAt: now.Add(-time.Hour), State: "running"},
		{InstanceID: "i-healthy", RunnerID: "runner-healthy", CreatedAt: now.Add(-time.Hour), State: "running"},
	}}
	control := &fleetControlFixture{archiveError: archiveErr, observation: contracts.FleetPoolObservation{
		Runners:    []contracts.FleetRunner{{ID: "runner-healthy", Connected: true, FleetState: "evacuating", WorkspaceCount: 1}},
		Workspaces: []contracts.FleetWorkspace{{SandboxID: "saved-workspace", RunnerID: "runner-healthy", State: "stopped", DesiredState: "stopped"}},
	}}
	_, err := testFleetManager(cloud, control).Reconcile(t.Context(), now)
	if !errors.Is(err, archiveErr) || !strings.Contains(err.Error(), "i-unregistered") {
		t.Fatalf("independent failure hidden: err=%v", err)
	}
	if len(control.actions) != 1 || control.actions[0] != "archive:saved-workspace" || len(cloud.terminated) != 0 {
		t.Fatalf("archive was skipped or unsafe termination followed refusal: actions=%v terminated=%v", control.actions, cloud.terminated)
	}
}

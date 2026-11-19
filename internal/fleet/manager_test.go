package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type fleetCloudFixture struct {
	nodes      []Node
	desired    []int
	terminated []string
}

func (f *fleetCloudFixture) ListNodes(context.Context) ([]Node, error) { return f.nodes, nil }
func (f *fleetCloudFixture) SetDesired(_ context.Context, n int) error {
	f.desired = append(f.desired, n)
	return nil
}
func (f *fleetCloudFixture) Terminate(_ context.Context, n Node) error {
	f.terminated = append(f.terminated, n.InstanceID)
	return nil
}

type fleetControlFixture struct {
	observation  contracts.FleetPoolObservation
	actions      []string
	archiveError error
}

func (f *fleetControlFixture) Observe(context.Context) (contracts.FleetPoolObservation, error) {
	return f.observation, nil
}
func (f *fleetControlFixture) Evacuate(_ context.Context, r contracts.FleetRunner) error {
	f.actions = append(f.actions, "evacuate:"+r.ID)
	return nil
}
func (f *fleetControlFixture) Retire(_ context.Context, r contracts.FleetRunner) error {
	f.actions = append(f.actions, "retire:"+r.ID)
	return nil
}
func (f *fleetControlFixture) Archive(_ context.Context, w contracts.FleetWorkspace) error {
	f.actions = append(f.actions, "archive:"+w.SandboxID)
	return f.archiveError
}
func testFleetManager(cloud Cloud, control ControlPlane) Manager {
	return Manager{Config: ManagerConfig{MaximumNodes: 2, NodeVCPUCount: 4, NodeMemoryBytes: 8 << 30, NodeInstances: 2, NodeWorkspaceBytes: 64 << 30, ScaleInAfter: 5 * time.Minute, MaximumArchivesPerPass: 4, BootstrapTimeout: 30 * time.Minute}, Cloud: cloud, Control: control}
}

func TestFleetColdDemandScalesWithoutARegisteredRunner(t *testing.T) {
	cloud := &fleetCloudFixture{}
	control := &fleetControlFixture{observation: contracts.FleetPoolObservation{Demand: contracts.FleetResourceDemand{Instances: 3, VCPUCount: 6, MemoryBytes: 12 << 30}}}
	result, err := testFleetManager(cloud, control).Reconcile(t.Context(), time.Now())
	if err != nil || result.Action != "scale_out" || len(cloud.desired) != 1 || cloud.desired[0] != 2 {
		t.Fatalf("result=%+v desired=%v err=%v", result, cloud.desired, err)
	}
	if len(control.actions) != 0 {
		t.Fatal("cold demand mutated workspace authority")
	}
}

func TestFleetRespectsOperatorCapacityBeyondQualificationSize(t *testing.T) {
	cloud := &fleetCloudFixture{}
	control := &fleetControlFixture{observation: contracts.FleetPoolObservation{Demand: contracts.FleetResourceDemand{Instances: 12, VCPUCount: 24, MemoryBytes: 48 << 30}}}
	manager := testFleetManager(cloud, control)
	manager.Config.MaximumNodes = 5
	result, err := manager.Reconcile(t.Context(), time.Now())
	if err != nil || result.Action != "scale_out" || result.DesiredNodes != 5 || len(cloud.desired) != 1 || cloud.desired[0] != 5 {
		t.Fatalf("operator ceiling not applied: result=%+v desired=%v err=%v", result, cloud.desired, err)
	}
	if len(control.actions) != 0 || len(cloud.terminated) != 0 {
		t.Fatalf("capacity expansion mutated existing authority: actions=%v terminated=%v", control.actions, cloud.terminated)
	}
}

func TestFleetInflightCloudInstancesPreventDuplicateScaleOut(t *testing.T) {
	now := time.Now()
	cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-pending", RunnerID: "runner-i-pending", CreatedAt: now, State: "pending"}}}
	control := &fleetControlFixture{observation: contracts.FleetPoolObservation{Demand: contracts.FleetResourceDemand{Instances: 1, VCPUCount: 2, MemoryBytes: 4 << 30}}}
	if _, err := testFleetManager(cloud, control).Reconcile(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(cloud.desired) != 0 || len(cloud.terminated) != 0 {
		t.Fatal("in-flight instance was ignored or terminated")
	}
}

func TestFleetDoesNotTerminateBeforeArchiveAndRetirement(t *testing.T) {
	now := time.Now()
	cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-old", RunnerID: "runner-i-old", CreatedAt: now.Add(-time.Hour)}}}
	control := &fleetControlFixture{observation: contracts.FleetPoolObservation{Runners: []contracts.FleetRunner{{ID: "runner-i-old", Connected: true, FleetState: "evacuating", WorkspaceCount: 1}}, Workspaces: []contracts.FleetWorkspace{{SandboxID: "saved", RunnerID: "runner-i-old", State: "stopped", DesiredState: "stopped"}}}, archiveError: errors.New("object store denied")}
	if _, err := testFleetManager(cloud, control).Reconcile(t.Context(), now); err == nil {
		t.Fatal("archive refusal hidden")
	}
	if len(cloud.terminated) != 0 {
		t.Fatal("archive failure terminated worker")
	}
	control.archiveError = nil
	control.observation.Workspaces = nil
	control.observation.Runners[0].WorkspaceCount = 0
	control.observation.Runners[0].FleetState = "evacuated"
	if _, err := testFleetManager(cloud, control).Reconcile(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(cloud.terminated) != 0 || control.actions[len(control.actions)-1] != "retire:runner-i-old" {
		t.Fatal("termination did not wait for retirement commit")
	}
	control.observation.Runners[0].FleetState = "retired"
	if _, err := testFleetManager(cloud, control).Reconcile(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(cloud.terminated) != 1 {
		t.Fatal("retired worker not terminated")
	}
}

func TestFleetPreservesActiveAndSnapshotOwningWorkers(t *testing.T) {
	now := time.Now()
	for _, runner := range []contracts.FleetRunner{
		{ID: "runner", Connected: true, FleetState: "active", ActiveAssignments: 1},
		{ID: "runner", Connected: true, FleetState: "active", SnapshotCount: 1},
		{ID: "runner", Connected: true, FleetState: "active", PendingMutations: 1},
	} {
		cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-safe", RunnerID: "runner", CreatedAt: now.Add(-time.Hour)}}}
		control := &fleetControlFixture{observation: contracts.FleetPoolObservation{Runners: []contracts.FleetRunner{runner}}}
		if _, err := testFleetManager(cloud, control).Reconcile(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		if len(control.actions) != 0 || len(cloud.terminated) != 0 {
			t.Fatalf("unsafe retirement for %+v", runner)
		}
	}
}

func TestFleetDiskPressureProvisionsAdditionalCapacity(t *testing.T) {
	now := time.Now()
	cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-full", RunnerID: "runner-full", CreatedAt: now.Add(-time.Hour)}}}
	control := &fleetControlFixture{observation: contracts.FleetPoolObservation{
		Demand:  contracts.FleetResourceDemand{Instances: 1, VCPUCount: 2, MemoryBytes: 4 << 30, WorkspaceBytes: 20 << 30},
		Runners: []contracts.FleetRunner{{ID: "runner-full", Connected: true, State: "ready", FleetState: "active", StorageAdmissionDenied: true, Allocatable: contracts.PoolComputeCapacity{Instances: 2, VCPUCount: 4, MemoryBytes: 8 << 30}, WorkspaceCapacityBytes: 64 << 30}},
	}}
	result, err := testFleetManager(cloud, control).Reconcile(t.Context(), now)
	if err != nil || result.Action != "scale_out" || len(cloud.desired) != 1 || cloud.desired[0] != 2 {
		t.Fatalf("result=%+v desired=%v err=%v", result, cloud.desired, err)
	}
}

func TestFleetScaleInWaitsSinceLastWorkspaceActivity(t *testing.T) {
	now := time.Now()
	recent := now.Add(-time.Minute)
	cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-idle", RunnerID: "runner-idle", CreatedAt: now.Add(-time.Hour)}}}
	control := &fleetControlFixture{observation: contracts.FleetPoolObservation{Runners: []contracts.FleetRunner{{ID: "runner-idle", Connected: true, FleetState: "active", LastActivityAt: &recent}}}}
	if _, err := testFleetManager(cloud, control).Reconcile(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(control.actions) != 0 {
		t.Fatal("recently active worker evacuated")
	}
	recent = now.Add(-10 * time.Minute)
	if _, err := testFleetManager(cloud, control).Reconcile(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(control.actions) != 1 || control.actions[0] != "evacuate:runner-idle" {
		t.Fatalf("actions=%v", control.actions)
	}
}

func TestFleetNeverRegisteredNodeRequiresBoundedOperatorRecovery(t *testing.T) {
	now := time.Now()
	cloud := &fleetCloudFixture{nodes: []Node{{InstanceID: "i-stale", RunnerID: "runner-i-stale", CreatedAt: now.Add(-time.Hour), State: "running"}}}
	control := &fleetControlFixture{observation: contracts.FleetPoolObservation{Demand: contracts.FleetResourceDemand{Instances: 1, VCPUCount: 2, MemoryBytes: 4 << 30}}}
	manager := testFleetManager(cloud, control)
	manager.Config.MaximumNodes = 1
	if _, err := manager.Reconcile(t.Context(), now); err == nil || !strings.Contains(err.Error(), "operator recovery is required") {
		t.Fatalf("bootstrap error=%v", err)
	}
	if len(cloud.desired) != 0 || len(cloud.terminated) != 0 || len(control.actions) != 0 {
		t.Fatal("stale unregistered node was automatically replaced or lost data")
	}
}

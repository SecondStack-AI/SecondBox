// Package fleet reconciles admitted Workspace demand with a bounded cloud fleet.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

type Node struct {
	InstanceID string
	RunnerID   string
	CreatedAt  time.Time
	State      string
}

type Cloud interface {
	ListNodes(context.Context) ([]Node, error)
	SetDesired(context.Context, int) error
	Terminate(context.Context, Node) error
}

type ControlPlane interface {
	Observe(context.Context) (contracts.FleetPoolObservation, error)
	Evacuate(context.Context, contracts.FleetRunner) error
	Archive(context.Context, contracts.FleetWorkspace) error
	Retire(context.Context, contracts.FleetRunner) error
}

type ManagerConfig struct {
	MinimumNodes           int
	MaximumNodes           int
	NodeVCPUCount          int64
	NodeMemoryBytes        int64
	NodeInstances          int64
	NodeWorkspaceBytes     int64
	ScaleInAfter           time.Duration
	MaximumArchivesPerPass int
	BootstrapTimeout       time.Duration
}

type ReconcileResult struct {
	ObservedNodes int    `json:"observedNodes"`
	DesiredNodes  int    `json:"desiredNodes"`
	Action        string `json:"action"`
	RunnerID      string `json:"runnerId,omitempty"`
}

type Manager struct {
	Config  ManagerConfig
	Cloud   Cloud
	Control ControlPlane
}

func (manager Manager) Validate() error {
	c := manager.Config
	if manager.Cloud == nil || manager.Control == nil || c.MinimumNodes < 0 || c.MaximumNodes < 1 ||
		c.MinimumNodes > c.MaximumNodes || c.NodeVCPUCount < 1 || c.NodeMemoryBytes < 1 ||
		c.NodeInstances < 1 || c.NodeWorkspaceBytes < 1 || c.ScaleInAfter <= 0 || c.BootstrapTimeout <= 0 || c.MaximumArchivesPerPass < 1 {
		return errors.New("SecondBox fleet requires explicit cloud, capacity and reconciliation bounds")
	}
	return nil
}

func (manager Manager) Reconcile(ctx context.Context, now time.Time) (ReconcileResult, error) {
	if err := manager.Validate(); err != nil {
		return ReconcileResult{}, err
	}
	observation, err := manager.Control.Observe(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	if observation.Truncated {
		return ReconcileResult{}, errors.New("SecondBox fleet observation exceeds its bound; narrow the RunnerPool")
	}
	if cleaner, ok := manager.Cloud.(interface{ CleanupRetiredVolumes(context.Context) error }); ok {
		if err := cleaner.CleanupRetiredVolumes(ctx); err != nil {
			return ReconcileResult{}, err
		}
	}
	nodes, err := manager.Cloud.ListNodes(ctx)
	if err != nil {
		return ReconcileResult{}, err
	}
	result := ReconcileResult{ObservedNodes: len(nodes), Action: "stable"}
	knownRunners := make(map[string]bool, len(observation.Runners))
	for _, runner := range observation.Runners {
		knownRunners[runner.ID] = true
	}
	for _, node := range nodes {
		if !knownRunners[node.RunnerID] && now.Sub(node.CreatedAt) > manager.Config.BootstrapTimeout {
			return result, fmt.Errorf("SecondBox fleet instance %s did not register within bootstrap timeout; operator recovery is required before replacement", node.InstanceID)
		}
	}
	var usedCPU, usedMemory, usedInstances int64
	for _, runner := range observation.Runners {
		if runner.FleetState == "active" {
			usedCPU += runner.Reserved.VCPUCount
			usedMemory += runner.Reserved.MemoryBytes
			usedInstances += runner.Reserved.Instances
		}
	}
	if observation.Demand.VCPUCount > math.MaxInt64-usedCPU || observation.Demand.MemoryBytes > math.MaxInt64-usedMemory ||
		observation.Demand.Instances > math.MaxInt64-usedInstances {
		return result, errors.New("SecondBox fleet demand exceeds numeric bounds")
	}
	desired := max(manager.Config.MinimumNodes,
		int(ceilingRatio(usedCPU+observation.Demand.VCPUCount, manager.Config.NodeVCPUCount)),
		int(ceilingRatio(usedMemory+observation.Demand.MemoryBytes, manager.Config.NodeMemoryBytes)),
		int(ceilingRatio(usedInstances+observation.Demand.Instances, manager.Config.NodeInstances)))
	result.DesiredNodes = min(desired, manager.Config.MaximumNodes)
	if observation.Demand.Instances > 0 {
		var freeCPU, freeMemory, freeInstances, freeDisk int64
		runnerByID := map[string]contracts.FleetRunner{}
		for _, runner := range observation.Runners {
			runnerByID[runner.ID] = runner
		}
		for _, node := range nodes {
			runner, known := runnerByID[node.RunnerID]
			if !known {
				// Instances still enrolling count as in-flight capacity.
				freeCPU += manager.Config.NodeVCPUCount
				freeMemory += manager.Config.NodeMemoryBytes
				freeInstances += manager.Config.NodeInstances
				freeDisk += manager.Config.NodeWorkspaceBytes
				continue
			}
			if runner.FleetState != "active" || !runner.Connected || runner.State != "ready" || runner.StorageAdmissionDenied {
				continue
			}
			freeCPU += max(0, runner.Allocatable.VCPUCount-runner.Reserved.VCPUCount)
			freeMemory += max(0, runner.Allocatable.MemoryBytes-runner.Reserved.MemoryBytes)
			freeInstances += max(0, runner.Allocatable.Instances-runner.Reserved.Instances)
			freeDisk += max(0, runner.WorkspaceCapacityBytes-runner.ReservedWorkspaceBytes)
		}
		additional := max(ceilingRatio(observation.Demand.VCPUCount-freeCPU, manager.Config.NodeVCPUCount),
			ceilingRatio(observation.Demand.MemoryBytes-freeMemory, manager.Config.NodeMemoryBytes),
			ceilingRatio(observation.Demand.Instances-freeInstances, manager.Config.NodeInstances),
			ceilingRatio(observation.Demand.WorkspaceBytes-freeDisk, manager.Config.NodeWorkspaceBytes))
		desired = max(desired, len(nodes)+int(additional))
		result.DesiredNodes = min(desired, manager.Config.MaximumNodes)
	}
	if desired > manager.Config.MaximumNodes {
		result.Action = "capacity_ceiling"
	}
	if len(nodes) < result.DesiredNodes {
		if err := manager.Cloud.SetDesired(ctx, result.DesiredNodes); err != nil {
			return result, err
		}
		result.Action = "scale_out"
		return result, nil
	}
	runners := make(map[string]contracts.FleetRunner, len(observation.Runners))
	for _, runner := range observation.Runners {
		runners[runner.ID] = runner
	}
	// Finish an existing evacuation even if traffic arrived meanwhile. A new
	// request can provision capacity without making a sealed source writable.
	for _, node := range nodes {
		runner, known := runners[node.RunnerID]
		if known && runner.FleetState == "retired" {
			result.Action, result.RunnerID = "terminate", runner.ID
			return result, manager.Cloud.Terminate(ctx, node)
		}
		if known && runner.FleetState == "evacuated" {
			if err := manager.Control.Retire(ctx, runner); err != nil {
				return result, err
			}
			result.Action, result.RunnerID = "retire", runner.ID
			return result, nil
		}
		if known && runner.FleetState == "evacuating" {
			archived := 0
			for _, workspace := range observation.Workspaces {
				if workspace.RunnerID != runner.ID || workspace.State != "stopped" || workspace.DesiredState != "stopped" || workspace.Mutation != "" {
					continue
				}
				if err := manager.Control.Archive(ctx, workspace); err != nil {
					return result, err
				}
				archived++
				if archived >= manager.Config.MaximumArchivesPerPass {
					break
				}
			}
			result.Action, result.RunnerID = "evacuate", runner.ID
			return result, nil
		}
	}
	if len(nodes) <= result.DesiredNodes {
		return result, nil
	}
	sort.Slice(nodes, func(i, j int) bool {
		return runners[nodes[i].RunnerID].WorkspaceCount < runners[nodes[j].RunnerID].WorkspaceCount
	})
	for _, node := range nodes {
		runner, known := runners[node.RunnerID]
		lastActivity := node.CreatedAt
		if runner.LastActivityAt != nil && runner.LastActivityAt.After(lastActivity) {
			lastActivity = *runner.LastActivityAt
		}
		if !known || !runner.Connected || runner.FleetState != "active" || runner.ActiveAssignments != 0 ||
			runner.Reserved.Instances != 0 || runner.PendingMutations != 0 || runner.SnapshotCount != 0 ||
			now.Sub(lastActivity) < manager.Config.ScaleInAfter {
			continue
		}
		busy := false
		for _, workspace := range observation.Workspaces {
			if workspace.RunnerID == runner.ID && (workspace.State != "stopped" || workspace.DesiredState != "stopped" || workspace.Mutation != "") {
				busy = true
				break
			}
		}
		if busy {
			continue
		}
		if err := manager.Control.Evacuate(ctx, runner); err != nil {
			return result, fmt.Errorf("SecondBox fleet begin evacuation: %w", err)
		}
		result.Action, result.RunnerID = "begin_evacuation", runner.ID
		return result, nil
	}
	return result, nil
}

func ceilingRatio(value, divisor int64) int64 {
	if value <= 0 {
		return 0
	}
	return 1 + (value-1)/divisor
}

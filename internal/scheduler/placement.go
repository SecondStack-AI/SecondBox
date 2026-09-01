// Package scheduler selects compatible runners and persists fenced assignment authority.
package scheduler

import (
	"errors"
	"sort"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
)

var ErrNoCompatibleRunner = errors.New("SecondBox scheduler found no compatible runner")
var ErrHomeRunnerUnavailable = errors.New("SecondBox Sandbox home runner is unavailable")
var ErrEgressContextUnavailable = ports.ErrEgressContextUnavailable

const (
	DrainPhaseActive   = "active"
	DrainPhaseDraining = "draining"
)

// Capacity is runner compute that may be reserved by assignments.
type Capacity struct {
	VCPUCount   int64
	MemoryBytes int64
	DiskBytes   int64
	Instances   int64
	Operations  int64
}

// Requirements are immutable ProfileRevision placement constraints.
type Requirements struct {
	PoolName                 string
	Architecture             string
	RequiredCapabilities     []string
	Capacity                 Capacity
	GuestProtocolGeneration  uint32
	PreferredArtifactDigests []string
	EgressContext            *string
}

// MaterializationSnapshot is one revalidated local backend composition.
type MaterializationSnapshot struct {
	BackendKind     string `json:"backendKind"`
	Architecture    string `json:"architecture"`
	RuntimeDigest   string `json:"runtimeDigest"`
	ToolchainDigest string `json:"toolchainDigest"`
	Digest          string `json:"digest"`
}

// RunnerSnapshot is one durable registration and latest heartbeat view.
type RunnerSnapshot struct {
	ID                      string
	PoolName                string
	BackendKind             string
	Architecture            string
	Capabilities            map[string]bool
	Allocatable             Capacity
	Reserved                Capacity
	DrainPhase              string
	LastHeartbeatAt         time.Time
	ArtifactDigests         []string
	Materializations        []MaterializationSnapshot
	SupportedEgressContexts []string
	GuestProtocolMinimum    uint32
	GuestProtocolMaximum    uint32
}

// SelectRunner applies hard compatibility before locality and stable identity ordering.
func SelectRunner(
	requirements Requirements,
	runners []RunnerSnapshot,
	now time.Time,
	heartbeatTimeout time.Duration,
) (RunnerSnapshot, error) {
	type rankedRunner struct {
		runner           RunnerSnapshot
		artifactLocality int
	}
	ranked := make([]rankedRunner, 0, len(runners))
	contextMismatch := false
	for _, runner := range runners {
		if !compatible(requirements, runner, now, heartbeatTimeout) {
			continue
		}
		if requirements.EgressContext != nil &&
			!contains(runner.SupportedEgressContexts, *requirements.EgressContext) {
			contextMismatch = true
			continue
		}
		ranked = append(ranked, rankedRunner{
			runner:           runner,
			artifactLocality: intersectionCount(runner.ArtifactDigests, requirements.PreferredArtifactDigests),
		})
	}
	if len(ranked) == 0 {
		if contextMismatch {
			return RunnerSnapshot{}, ErrEgressContextUnavailable
		}
		return RunnerSnapshot{}, ErrNoCompatibleRunner
	}
	sort.Slice(ranked, func(left, right int) bool {
		if ranked[left].artifactLocality != ranked[right].artifactLocality {
			return ranked[left].artifactLocality > ranked[right].artifactLocality
		}
		leftFree := freeCapacity(ranked[left].runner)
		rightFree := freeCapacity(ranked[right].runner)
		if leftFree.Instances != rightFree.Instances {
			return leftFree.Instances > rightFree.Instances
		}
		if leftFree.VCPUCount != rightFree.VCPUCount {
			return leftFree.VCPUCount > rightFree.VCPUCount
		}
		if leftFree.MemoryBytes != rightFree.MemoryBytes {
			return leftFree.MemoryBytes > rightFree.MemoryBytes
		}
		return ranked[left].runner.ID < ranked[right].runner.ID
	})
	return ranked[0].runner, nil
}

// SelectHomeRunner admits only the current authoritative home Runner. Compatible
// non-home Runners are deliberately invisible to ordinary lifecycle placement.
func SelectHomeRunner(
	homeRunnerID string,
	requirements Requirements,
	runners []RunnerSnapshot,
	now time.Time,
	heartbeatTimeout time.Duration,
) (RunnerSnapshot, error) {
	if homeRunnerID == "" {
		return RunnerSnapshot{}, ErrHomeRunnerUnavailable
	}
	homeCandidates := make([]RunnerSnapshot, 0, 1)
	for _, runner := range runners {
		if runner.ID == homeRunnerID {
			homeCandidates = append(homeCandidates, runner)
			break
		}
	}
	selected, err := SelectRunner(requirements, homeCandidates, now, heartbeatTimeout)
	if errors.Is(err, ErrEgressContextUnavailable) {
		return RunnerSnapshot{}, err
	}
	if errors.Is(err, ErrNoCompatibleRunner) {
		return RunnerSnapshot{}, ErrHomeRunnerUnavailable
	}
	return selected, err
}

func compatible(
	requirements Requirements,
	runner RunnerSnapshot,
	now time.Time,
	heartbeatTimeout time.Duration,
) bool {
	if runner.PoolName != requirements.PoolName ||
		runner.Architecture != requirements.Architecture ||
		runner.DrainPhase != DrainPhaseActive ||
		runner.LastHeartbeatAt.Before(now.Add(-heartbeatTimeout)) {
		return false
	}
	if runner.BackendKind == "" || !runner.Capabilities["compute"] {
		return false
	}
	if len(requirements.PreferredArtifactDigests) != 2 || !hasMaterialization(runner, requirements.PreferredArtifactDigests[0], requirements.PreferredArtifactDigests[1]) {
		return false
	}
	if requirements.GuestProtocolGeneration == 0 ||
		requirements.GuestProtocolGeneration < runner.GuestProtocolMinimum ||
		requirements.GuestProtocolGeneration > runner.GuestProtocolMaximum {
		return false
	}
	for _, capability := range requirements.RequiredCapabilities {
		if !runner.Capabilities[capability] {
			return false
		}
	}
	for _, prerequisite := range []string{
		"network-policy", "storage", "cleanup", "local-workspace",
	} {
		if !runner.Capabilities[prerequisite] {
			return false
		}
	}
	free := freeCapacity(runner)
	return free.VCPUCount >= requirements.Capacity.VCPUCount &&
		free.MemoryBytes >= requirements.Capacity.MemoryBytes &&
		free.DiskBytes >= requirements.Capacity.DiskBytes &&
		free.Instances >= requirements.Capacity.Instances &&
		free.Operations >= requirements.Capacity.Operations
}

func hasMaterialization(runner RunnerSnapshot, runtimeDigest, toolchainDigest string) bool {
	for _, candidate := range runner.Materializations {
		if candidate.BackendKind == runner.BackendKind && candidate.Architecture == runner.Architecture &&
			candidate.RuntimeDigest == runtimeDigest && candidate.ToolchainDigest == toolchainDigest && candidate.Digest != "" {
			return true
		}
	}
	return false
}

func freeCapacity(runner RunnerSnapshot) Capacity {
	return Capacity{
		VCPUCount:   runner.Allocatable.VCPUCount - runner.Reserved.VCPUCount,
		MemoryBytes: runner.Allocatable.MemoryBytes - runner.Reserved.MemoryBytes,
		DiskBytes:   runner.Allocatable.DiskBytes - runner.Reserved.DiskBytes,
		Instances:   runner.Allocatable.Instances - runner.Reserved.Instances,
		Operations:  runner.Allocatable.Operations - runner.Reserved.Operations,
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func intersectionCount(left, right []string) int {
	count := 0
	for _, value := range left {
		if contains(right, value) {
			count++
		}
	}
	return count
}

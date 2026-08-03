// Package scheduler selects compatible runners and persists fenced assignment authority.
package scheduler

import (
	"errors"
	"sort"
	"time"
)

var ErrNoCompatibleRunner = errors.New("SecondBox scheduler found no compatible runner")
var ErrHomeRunnerUnavailable = errors.New("SecondBox Sandbox home runner is unavailable")

const (
	DrainPhaseActive   = "active"
	DrainPhaseDraining = "draining"
	DrainPhaseDrained  = "drained"
)

// Capacity is runner compute that may be reserved by assignments.
type Capacity struct {
	CPUMillis   int64
	MemoryBytes int64
	DiskBytes   int64
	Instances   int64
	Operations  int64
}

// Requirements are immutable ProfileRevision placement constraints.
type Requirements struct {
	PoolName                 string
	BackendKind              string
	Architecture             string
	RequiredCapabilities     []string
	Capacity                 Capacity
	GuestProtocolGeneration  uint32
	PreferredArtifactDigests []string
}

// RunnerSnapshot is one durable registration and latest heartbeat view.
type RunnerSnapshot struct {
	ID                   string
	PoolName             string
	Architecture         string
	Capabilities         map[string]bool
	Allocatable          Capacity
	Reserved             Capacity
	DrainPhase           string
	LastHeartbeatAt      time.Time
	ArtifactDigests      []string
	GuestProtocolMinimum uint32
	GuestProtocolMaximum uint32
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
	for _, runner := range runners {
		if !compatible(requirements, runner, now, heartbeatTimeout) {
			continue
		}
		ranked = append(ranked, rankedRunner{
			runner:           runner,
			artifactLocality: intersectionCount(runner.ArtifactDigests, requirements.PreferredArtifactDigests),
		})
	}
	if len(ranked) == 0 {
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
		if leftFree.CPUMillis != rightFree.CPUMillis {
			return leftFree.CPUMillis > rightFree.CPUMillis
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
	if requirements.BackendKind != "firecracker" || !runner.Capabilities["compute"] {
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
	return free.CPUMillis >= requirements.Capacity.CPUMillis &&
		free.MemoryBytes >= requirements.Capacity.MemoryBytes &&
		free.DiskBytes >= requirements.Capacity.DiskBytes &&
		free.Instances >= requirements.Capacity.Instances &&
		free.Operations >= requirements.Capacity.Operations
}

func freeCapacity(runner RunnerSnapshot) Capacity {
	return Capacity{
		CPUMillis:   runner.Allocatable.CPUMillis - runner.Reserved.CPUMillis,
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

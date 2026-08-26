package store

import (
	"context"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

type runnerPlacementOptions struct {
	exactRunnerID            string
	excludedRunnerID         string
	requireWorkspaceTransfer bool
	unavailable              error
	errorPrefix              string
}

type runnerPlacementCandidate struct {
	id                 string
	poolName           string
	state              string
	drainPhase         string
	activeConnectionID string
	backendKind        string
	architectures      []string
	capabilities       []string
	protocolVersions   []string
	materializations   []placementMaterialization
	allocatable        runnerCapacity
	reportedReserved   runnerCapacity
}

// placementMaterialization mirrors the scheduler's materialization snapshot
// shape recorded in artifact_cache_json.
type placementMaterialization struct {
	BackendKind     string `json:"backendKind"`
	Architecture    string `json:"architecture"`
	RuntimeDigest   string `json:"runtimeDigest"`
	ToolchainDigest string `json:"toolchainDigest"`
	Digest          string `json:"digest"`
}

// selectRunnerForPlacement ranks an unlocked snapshot, then locks and
// revalidates one candidate at a time. Every durable home writer uses the
// selected Runner row as its admission serialization point.
func selectRunnerForPlacement(
	ctx context.Context,
	tx pgx.Tx,
	spec contracts.ProfileRevisionSpec,
	options runnerPlacementOptions,
) (string, error) {
	rows, err := tx.Query(ctx, `
		SELECT id,pool_name,state,drain_phase,active_connection_id,backend_kind,
		       architectures_json,capabilities_json,protocol_versions_json,
		       capacity_json,reserved_capacity_json,artifact_cache_json
		FROM secondbox.runners
		WHERE pool_name=$1
		  AND ($2='' OR id=$2)
		  AND ($3='' OR id<>$3)
		ORDER BY id`,
		spec.Pool, options.exactRunnerID, options.excludedRunnerID,
	)
	if err != nil {
		return "", fmt.Errorf("%s candidates failed: %w", options.errorPrefix, err)
	}
	candidates := make([]runnerPlacementCandidate, 0)
	for rows.Next() {
		candidate, scanErr := scanRunnerPlacementCandidate(
			rows, options.errorPrefix, options.requireWorkspaceTransfer,
		)
		if scanErr != nil {
			rows.Close()
			return "", fmt.Errorf("%s candidate scan failed: %w", options.errorPrefix, scanErr)
		}
		if options.exactRunnerID != "" ||
			runnerPlacementCompatible(candidate, spec, options, candidate.reportedReserved) {
			candidates = append(candidates, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("%s candidate iteration failed: %w", options.errorPrefix, err)
	}
	rows.Close()

	if options.exactRunnerID != "" {
		if len(candidates) == 0 {
			return "", options.unavailable
		}
		selected, locked, err := lockRunnerPlacementCandidate(
			ctx, tx, candidates[0], spec, options, false,
		)
		if err != nil {
			return "", err
		}
		if locked && selected != "" {
			return selected, nil
		}
		return "", options.unavailable
	}

	lockedCandidates := 0
	for _, snapshot := range candidates {
		selected, locked, err := lockRunnerPlacementCandidate(
			ctx, tx, snapshot, spec, options, true,
		)
		if err != nil {
			return "", err
		}
		if !locked {
			continue
		}
		lockedCandidates++
		if selected != "" {
			return selected, nil
		}
	}
	if lockedCandidates == 0 && len(candidates) > 0 {
		// A compatible fleet must not become unavailable merely because every
		// candidate was momentarily locked. Wait for the first ranked candidate
		// and revalidate so the worst case preserves the old bounded contention.
		selected, locked, err := lockRunnerPlacementCandidate(
			ctx, tx, candidates[0], spec, options, false,
		)
		if err != nil {
			return "", err
		}
		if locked && selected != "" {
			return selected, nil
		}
	}
	return "", options.unavailable
}

func lockRunnerPlacementCandidate(
	ctx context.Context,
	tx pgx.Tx,
	snapshot runnerPlacementCandidate,
	spec contracts.ProfileRevisionSpec,
	options runnerPlacementOptions,
	skipLocked bool,
) (string, bool, error) {
	lockClause := "FOR UPDATE"
	if skipLocked {
		lockClause += " SKIP LOCKED"
	}
	candidate, err := scanRunnerPlacementCandidate(tx.QueryRow(ctx, `
			SELECT id,pool_name,state,drain_phase,active_connection_id,backend_kind,
			       architectures_json,capabilities_json,protocol_versions_json,
			       capacity_json,reserved_capacity_json,artifact_cache_json
			FROM secondbox.runners
			WHERE id=$1
			`+lockClause, snapshot.id),
		options.errorPrefix,
		options.requireWorkspaceTransfer,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%s candidate lock failed: %w", options.errorPrefix, err)
	}
	durableReserved, err := durableHomeReservation(ctx, tx, candidate.id)
	if err != nil {
		return "", true, fmt.Errorf("%s durable reservation failed: %w", options.errorPrefix, err)
	}
	effectiveReserved := maxRunnerPlacementCapacity(
		candidate.reportedReserved,
		durableReserved,
	)
	if runnerPlacementCompatible(candidate, spec, options, effectiveReserved) {
		return candidate.id, true, nil
	}
	return "", true, nil
}

func scanRunnerPlacementCandidate(
	scanner interface{ Scan(dest ...any) error },
	errorPrefix string,
	decodeProtocolVersions bool,
) (runnerPlacementCandidate, error) {
	var candidate runnerPlacementCandidate
	var architecturesJSON, capabilitiesJSON, versionsJSON, capacityJSON, reservedJSON, cacheJSON []byte
	if err := scanner.Scan(
		&candidate.id, &candidate.poolName, &candidate.state,
		&candidate.drainPhase, &candidate.activeConnectionID, &candidate.backendKind,
		&architecturesJSON, &capabilitiesJSON, &versionsJSON,
		&capacityJSON, &reservedJSON, &cacheJSON,
	); err != nil {
		return runnerPlacementCandidate{}, err
	}
	materializations, err := decodeArtifactCacheEvidence(cacheJSON)
	if err != nil {
		return runnerPlacementCandidate{}, fmt.Errorf("%s %w", errorPrefix, err)
	}
	candidate.materializations = materializations
	for _, item := range []struct {
		name  string
		value []byte
		dest  any
	}{
		{name: "architectures", value: architecturesJSON, dest: &candidate.architectures},
		{name: "capabilities", value: capabilitiesJSON, dest: &candidate.capabilities},
		{name: "capacity", value: capacityJSON, dest: &candidate.allocatable},
		{name: "reservation", value: reservedJSON, dest: &candidate.reportedReserved},
	} {
		if err := json.Unmarshal(item.value, item.dest); err != nil {
			return runnerPlacementCandidate{}, fmt.Errorf("%s %s decoding failed: %w", errorPrefix, item.name, err)
		}
	}
	if decodeProtocolVersions {
		if err := json.Unmarshal(versionsJSON, &candidate.protocolVersions); err != nil {
			return runnerPlacementCandidate{}, fmt.Errorf(
				"%s protocol versions decoding failed: %w",
				errorPrefix,
				err,
			)
		}
	}
	return candidate, nil
}

func runnerPlacementCompatible(
	candidate runnerPlacementCandidate,
	spec contracts.ProfileRevisionSpec,
	options runnerPlacementOptions,
	reserved runnerCapacity,
) bool {
	if candidate.poolName != spec.Pool || candidate.state != "ready" ||
		candidate.drainPhase != "active" || candidate.activeConnectionID == "" ||
		!contains(candidate.architectures, spec.Architecture) ||
		!contains(candidate.capabilities, "compute") ||
		!contains(candidate.capabilities, "local-workspace") {
		return false
	}
	if options.requireWorkspaceTransfer &&
		(!contains(candidate.capabilities, "storage") ||
			!contains(candidate.capabilities, "workspace-relocation") ||
			!supportsProtocolGeneration(candidate.protocolVersions, 2)) {
		return false
	}
	// A Sandbox never leaves its home Runner, so the startup mode a Profile pins
	// has to be satisfiable by the Runner chosen here, at creation, and by every
	// later Instance assignment onto that same Runner.
	if spec.Startup.Mode == contracts.StartupModeSnapshotResume &&
		!contains(candidate.capabilities, contracts.RunnerCapabilitySnapshotResume) {
		return false
	}
	// A Sandbox is homed permanently, so the home must already hold an exact
	// materialization of the Profile's pinned execution assets for its sealed
	// backend; otherwise every later assignment onto this home is refused.
	if !placementHasMaterialization(candidate, spec) {
		return false
	}
	return candidate.allocatable.VCPUCount-reserved.VCPUCount >= spec.Resources.VCPUCount &&
		candidate.allocatable.MemoryBytes-reserved.MemoryBytes >= spec.Resources.MemoryBytes &&
		candidate.allocatable.DiskBytes-reserved.DiskBytes >= spec.Resources.WorkspaceBytes &&
		candidate.allocatable.Instances-reserved.Instances >= 1 &&
		candidate.allocatable.Operations-reserved.Operations >= spec.Resources.ConcurrentOperations
}

// decodeArtifactCacheEvidence reads a runner's artifact cache. The legacy
// bare digest-array shape means "no proven materializations" and keeps the
// candidate incompatible until it re-registers; the current shape is an
// object carrying at least one recognized evidence field with its exact
// type. Anything else — null, an empty object, unknown-only fields, or a
// wrong field type — is corrupted evidence and surfaces as an error rather
// than reading as ordinary unavailability.
func decodeArtifactCacheEvidence(cacheJSON []byte) ([]placementMaterialization, error) {
	trimmed := bytes.TrimSpace(cacheJSON)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("artifact cache evidence is empty")
	}
	if trimmed[0] == '[' {
		var legacyDigests []string
		if err := json.Unmarshal(trimmed, &legacyDigests); err != nil {
			return nil, fmt.Errorf("artifact cache evidence is malformed: %w", err)
		}
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil || raw == nil {
		return nil, fmt.Errorf("artifact cache evidence is malformed: %w", err)
	}
	recognized := false
	var materializations []placementMaterialization
	if digests, exists := raw["artifactDigests"]; exists {
		var decoded []string
		if err := json.Unmarshal(digests, &decoded); err != nil {
			return nil, fmt.Errorf("artifact cache digest evidence is malformed: %w", err)
		}
		recognized = true
	}
	if entries, exists := raw["materializations"]; exists {
		if err := json.Unmarshal(entries, &materializations); err != nil {
			return nil, fmt.Errorf("artifact cache materialization evidence is malformed: %w", err)
		}
		recognized = true
	}
	if !recognized {
		return nil, fmt.Errorf("artifact cache evidence carries no recognized fields")
	}
	return materializations, nil
}

func placementHasMaterialization(candidate runnerPlacementCandidate, spec contracts.ProfileRevisionSpec) bool {
	if candidate.backendKind == "" {
		return false
	}
	for _, materialization := range candidate.materializations {
		if materialization.BackendKind == candidate.backendKind &&
			materialization.Architecture == spec.Architecture &&
			materialization.RuntimeDigest == spec.RuntimeBundleDigest &&
			materialization.ToolchainDigest == spec.ToolchainBundleDigest &&
			materialization.Digest != "" {
			return true
		}
	}
	return false
}

func supportsProtocolGeneration(versions []string, minimum uint64) bool {
	for _, version := range versions {
		parsed, err := strconv.ParseUint(version, 10, 32)
		if err == nil && parsed >= minimum {
			return true
		}
	}
	return false
}

func durableHomeReservation(ctx context.Context, tx pgx.Tx, runnerID string) (runnerCapacity, error) {
	var reserved runnerCapacity
	if err := tx.QueryRow(ctx, `
		WITH reservations AS (
		  SELECT workspace.logical_capacity_bytes
		  FROM secondbox.workspaces AS workspace
		  JOIN secondbox.sandboxes AS sandbox ON sandbox.id=workspace.sandbox_id
		  WHERE workspace.home_runner_id=$1 AND sandbox.state<>'deleted'
		  UNION ALL
		  SELECT relocation.logical_capacity_bytes
		  FROM secondbox.workspace_relocations AS relocation
		  WHERE relocation.target_runner_id=$1
		    AND relocation.state IN ('queued','source_sealed','aborting')
		)
		SELECT COALESCE(sum(logical_capacity_bytes),0)
		FROM reservations`, runnerID,
	).Scan(&reserved.DiskBytes); err != nil {
		return runnerCapacity{}, err
	}
	return reserved, nil
}

func maxRunnerPlacementCapacity(left, right runnerCapacity) runnerCapacity {
	return runnerCapacity{
		VCPUCount:   max(left.VCPUCount, right.VCPUCount),
		MemoryBytes: max(left.MemoryBytes, right.MemoryBytes),
		DiskBytes:   max(left.DiskBytes, right.DiskBytes),
		Instances:   max(left.Instances, right.Instances),
		Operations:  max(left.Operations, right.Operations),
	}
}

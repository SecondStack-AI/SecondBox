package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	architectures      []string
	capabilities       []string
	protocolVersions   []string
	allocatable        runnerCapacity
	reportedReserved   runnerCapacity
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
		SELECT id,pool_name,state,drain_phase,active_connection_id,
		       architectures_json,capabilities_json,protocol_versions_json,
		       capacity_json,reserved_capacity_json
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
			SELECT id,pool_name,state,drain_phase,active_connection_id,
			       architectures_json,capabilities_json,protocol_versions_json,
			       capacity_json,reserved_capacity_json
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
	var architecturesJSON, capabilitiesJSON, versionsJSON, capacityJSON, reservedJSON []byte
	if err := scanner.Scan(
		&candidate.id, &candidate.poolName, &candidate.state,
		&candidate.drainPhase, &candidate.activeConnectionID,
		&architecturesJSON, &capabilitiesJSON, &versionsJSON,
		&capacityJSON, &reservedJSON,
	); err != nil {
		return runnerPlacementCandidate{}, err
	}
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
			!contains(candidate.protocolVersions, "2")) {
		return false
	}
	// A Sandbox never leaves its home Runner, so the startup mode a Profile pins
	// has to be satisfiable by the Runner chosen here, at creation, and by every
	// later Instance assignment onto that same Runner.
	if spec.Startup.Mode == contracts.StartupModeSnapshotResume &&
		!contains(candidate.capabilities, contracts.RunnerCapabilitySnapshotResume) {
		return false
	}
	return candidate.allocatable.CPUMillis-reserved.CPUMillis >= spec.Resources.CPUMillis &&
		candidate.allocatable.MemoryBytes-reserved.MemoryBytes >= spec.Resources.MemoryBytes &&
		candidate.allocatable.DiskBytes-reserved.DiskBytes >= spec.Resources.WorkspaceBytes &&
		candidate.allocatable.Instances-reserved.Instances >= 1 &&
		candidate.allocatable.Operations-reserved.Operations >= spec.Resources.ConcurrentOperations
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
		CPUMillis:   max(left.CPUMillis, right.CPUMillis),
		MemoryBytes: max(left.MemoryBytes, right.MemoryBytes),
		DiskBytes:   max(left.DiskBytes, right.DiskBytes),
		Instances:   max(left.Instances, right.Instances),
		Operations:  max(left.Operations, right.Operations),
	}
}

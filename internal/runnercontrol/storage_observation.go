package runnercontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func persistWorkspaceStorageObservations(ctx context.Context, tx pgx.Tx, heartbeat *runnerv1.RunnerHeartbeat, receivedAt time.Time) error {
	if len(heartbeat.WorkspaceStorage) > 64 {
		return fmt.Errorf("SecondBox Workspace storage observation batch exceeds 64")
	}
	seen := make(map[string]bool, len(heartbeat.WorkspaceStorage))
	for _, item := range heartbeat.WorkspaceStorage {
		if item == nil || item.WorkspaceId == "" || seen[item.WorkspaceId] || item.Generation > math.MaxInt64 {
			return fmt.Errorf("SecondBox Workspace storage observation identity is invalid")
		}
		seen[item.WorkspaceId] = true
		observedAt, err := storageObservationTime(item.ObservedAtUnixMs, receivedAt)
		if err != nil {
			return err
		}
		observation := contracts.WorkspaceStorageObservation{Status: "unavailable", ObservedAt: &observedAt, Reason: item.UnavailableReason}
		if item.AllocatedBytes != nil {
			if item.GetAllocatedBytes() > math.MaxInt64 || item.UnavailableReason != "" || item.Generation == 0 {
				return fmt.Errorf("SecondBox Workspace allocated storage observation is invalid")
			}
			allocated := int64(item.GetAllocatedBytes())
			observation.Status, observation.AllocatedBytes = "available", &allocated
		} else if item.UnavailableReason != "missing" && item.UnavailableReason != "probe_failed" {
			return fmt.Errorf("SecondBox Workspace storage observation reason is invalid")
		}
		encoded, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		// Telemetry must not wait behind Workspace mutations while holding the
		// Runner heartbeat lock. A later bounded observation revisits busy rows.
		_, err = tx.Exec(ctx, `WITH observed_workspace AS MATERIALIZED (
			SELECT id FROM secondbox.workspaces WHERE id=$1 FOR UPDATE SKIP LOCKED
		) UPDATE secondbox.workspaces AS workspace SET storage_observation_json=$4
			FROM observed_workspace WHERE workspace.id=observed_workspace.id AND home_runner_id=$2 AND state <> 'deleted'
			AND ($3::bigint=0 OR generation=$3)
			AND (storage_observation_json IS NULL OR (storage_observation_json->>'observedAt')::timestamptz <= $5)`, item.WorkspaceId, heartbeat.RunnerId, int64(item.Generation), encoded, observedAt)
		if err != nil {
			return fmt.Errorf("SecondBox Workspace storage observation persistence failed: %w", err)
		}
	}
	if pressure := heartbeat.StoragePressure; pressure != nil {
		switch pressure.Status {
		case "healthy", "warning", "admission_denied", "unavailable":
		default:
			return fmt.Errorf("SecondBox storage pressure observation status is invalid")
		}
		observedAt, err := storageObservationTime(pressure.ObservedAtUnixMs, receivedAt)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(contracts.StoragePressureObservation{Status: pressure.Status, ObservedAt: &observedAt})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE secondbox.runners SET storage_pressure_json=$3 WHERE id=$1 AND active_connection_id=$2
			AND (storage_pressure_json IS NULL OR (storage_pressure_json->>'observedAt')::timestamptz <= $4)`, heartbeat.RunnerId, heartbeat.ConnectionId, encoded, observedAt)
		if err != nil {
			return fmt.Errorf("SecondBox storage pressure observation persistence failed: %w", err)
		}
	}
	return nil
}

func storageObservationTime(milliseconds uint64, receivedAt time.Time) (time.Time, error) {
	if milliseconds == 0 || milliseconds > math.MaxInt64 {
		return time.Time{}, fmt.Errorf("SecondBox storage observation timestamp is invalid")
	}
	observedAt := time.UnixMilli(int64(milliseconds)).UTC()
	if observedAt.After(receivedAt.Add(time.Minute)) {
		return time.Time{}, fmt.Errorf("SecondBox storage observation timestamp is in the future")
	}
	return observedAt, nil
}

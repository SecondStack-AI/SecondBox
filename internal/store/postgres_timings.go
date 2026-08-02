package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SecondStack-AI/SecondBox/internal/observability"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

const startupTimingStagesSQL = `
	'runner_admission','artifact_verify','workspace_attach','network_setup',
	'compute_launch','guest_negotiation','ready'`

// ReadSandboxTiming returns one explicitly bounded subject-owned timing history.
func (store *PostgresControlPlaneStore) ReadSandboxTiming(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	limit int,
) (contracts.SandboxTiming, error) {
	var exists int
	if err := store.pool.QueryRow(ctx, `
		SELECT 1
		FROM secondbox.sandboxes
		WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3`,
		tenantRef, subjectRef, sandboxID,
	).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return contracts.SandboxTiming{}, ports.ErrSandboxNotFound
	} else if err != nil {
		return contracts.SandboxTiming{}, fmt.Errorf("SecondBox Sandbox timing ownership lookup failed: %w", err)
	}

	operations, err := store.readOperationTimingRows(
		ctx,
		`WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		 ORDER BY created_at DESC,id DESC LIMIT $4`,
		tenantRef, subjectRef, sandboxID, limit,
	)
	if err != nil {
		return contracts.SandboxTiming{}, err
	}
	if err := store.attachBootTimings(ctx, operations); err != nil {
		return contracts.SandboxTiming{}, err
	}
	if err := store.attachOperationStageTimings(ctx, operations); err != nil {
		return contracts.SandboxTiming{}, err
	}
	execs, err := store.readSandboxExecTimings(
		ctx, tenantRef, subjectRef, sandboxID, limit,
	)
	if err != nil {
		return contracts.SandboxTiming{}, err
	}
	return contracts.SandboxTiming{
		SandboxID: sandboxID, Operations: operations, Execs: execs,
	}, nil
}

// ReadOperationTiming returns subject-owned timing evidence for one Operation.
func (store *PostgresControlPlaneStore) ReadOperationTiming(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	operationID string,
) (contracts.OperationTiming, error) {
	operations, err := store.readOperationTimingRows(
		ctx,
		`WHERE tenant_ref=$1 AND subject_ref=$2 AND id=$3
		 ORDER BY created_at DESC,id DESC LIMIT 1`,
		tenantRef, subjectRef, operationID,
	)
	if err != nil {
		return contracts.OperationTiming{}, err
	}
	if len(operations) == 0 {
		return contracts.OperationTiming{}, ports.ErrSandboxNotFound
	}
	if err := store.attachBootTimings(ctx, operations); err != nil {
		return contracts.OperationTiming{}, err
	}
	if err := store.attachOperationStageTimings(ctx, operations); err != nil {
		return contracts.OperationTiming{}, err
	}
	return operations[0], nil
}

func (store *PostgresControlPlaneStore) readOperationTimingRows(
	ctx context.Context,
	where string,
	arguments ...any,
) ([]contracts.OperationTiming, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id,sandbox_id,kind,state,created_at,started_at,completed_at
		FROM secondbox.operations `+where, arguments...)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Operation timing list failed: %w", err)
	}
	defer rows.Close()
	operations := make([]contracts.OperationTiming, 0)
	for rows.Next() {
		var timing contracts.OperationTiming
		if err := rows.Scan(
			&timing.OperationID, &timing.SandboxID, &timing.Kind, &timing.State,
			&timing.CreatedAt, &timing.StartedAt, &timing.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("SecondBox Operation timing scan failed: %w", err)
		}
		timing.Boots = []contracts.BootTiming{}
		timing.Orchestration = []contracts.OperationStageTiming{}
		if timing.StartedAt != nil {
			timing.QueueMilliseconds = durationMilliseconds(timing.CreatedAt, *timing.StartedAt)
		}
		if timing.StartedAt != nil && timing.CompletedAt != nil {
			timing.ExecutionMilliseconds = durationMilliseconds(*timing.StartedAt, *timing.CompletedAt)
		}
		if timing.CompletedAt != nil {
			timing.TotalMilliseconds = durationMilliseconds(timing.CreatedAt, *timing.CompletedAt)
		}
		operations = append(operations, timing)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox Operation timing rows failed: %w", err)
	}
	return operations, nil
}

func (store *PostgresControlPlaneStore) attachOperationStageTimings(
	ctx context.Context,
	operations []contracts.OperationTiming,
) error {
	if len(operations) == 0 {
		return nil
	}
	operationIDs := make([]string, len(operations))
	operationIndexes := make(map[string]int, len(operations))
	for index := range operations {
		operationIDs[index] = operations[index].OperationID
		operationIndexes[operations[index].OperationID] = index
	}
	rows, err := store.pool.Query(ctx, `
		SELECT operation_id,stage,observed_at
		FROM secondbox.operation_stage_timings
		WHERE operation_id=ANY($1::text[])
		  AND stage IN (
		    'durable_admission','workspace_ready',
		    'placement_reconcile_started','placement_effect_started',
		    'placement_plan_ready','placement_schedule_started',
		    'placement_attempt_started','placement_sandbox_locked',
		    'placement_assignment_checked','placement_candidates_locked',
		    'placement_candidate_selected','placement_ready',
		    'startup_dispatched','ready_projected'
		  )
		ORDER BY operation_id,observed_at,
		  CASE stage
		    WHEN 'durable_admission' THEN 1
		    WHEN 'workspace_ready' THEN 2
		    WHEN 'placement_reconcile_started' THEN 3
		    WHEN 'placement_effect_started' THEN 4
		    WHEN 'placement_plan_ready' THEN 5
		    WHEN 'placement_schedule_started' THEN 6
		    WHEN 'placement_attempt_started' THEN 7
		    WHEN 'placement_sandbox_locked' THEN 8
		    WHEN 'placement_assignment_checked' THEN 9
		    WHEN 'placement_candidates_locked' THEN 10
		    WHEN 'placement_candidate_selected' THEN 11
		    WHEN 'placement_ready' THEN 12
		    WHEN 'startup_dispatched' THEN 13
		    WHEN 'ready_projected' THEN 14
		  END`,
		operationIDs,
	)
	if err != nil {
		return fmt.Errorf("SecondBox Operation stage timing list failed: %w", err)
	}
	defer rows.Close()
	previous := make(map[string]time.Time, len(operations))
	for rows.Next() {
		var operationID, stage string
		var observedAt time.Time
		if err := rows.Scan(&operationID, &stage, &observedAt); err != nil {
			return fmt.Errorf("SecondBox Operation stage timing scan failed: %w", err)
		}
		operationIndex, exists := operationIndexes[operationID]
		if !exists {
			return errors.New("SecondBox Operation stage timing references an unselected Operation")
		}
		start := operations[operationIndex].CreatedAt
		previousAt, hasPrevious := previous[operationID]
		if hasPrevious {
			start = previousAt
		}
		operations[operationIndex].Orchestration = append(
			operations[operationIndex].Orchestration,
			contracts.OperationStageTiming{
				Stage:               stage,
				ObservedAt:          observedAt,
				ElapsedMilliseconds: preciseDurationMilliseconds(start, observedAt),
				CumulativeMilliseconds: preciseDurationMilliseconds(
					operations[operationIndex].CreatedAt,
					observedAt,
				),
			},
		)
		previous[operationID] = observedAt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("SecondBox Operation stage timing rows failed: %w", err)
	}
	return nil
}

func durationMilliseconds(start, end time.Time) *int64 {
	value := max(end.Sub(start).Milliseconds(), 0)
	return &value
}

func preciseDurationMilliseconds(start, end time.Time) float64 {
	return max(float64(end.Sub(start))/float64(time.Millisecond), 0)
}

func (store *PostgresControlPlaneStore) attachBootTimings(
	ctx context.Context,
	operations []contracts.OperationTiming,
) error {
	if len(operations) == 0 {
		return nil
	}
	operationIDs := make([]string, len(operations))
	operationIndexes := make(map[string]int, len(operations))
	for index := range operations {
		operationIDs[index] = operations[index].OperationID
		operationIndexes[operations[index].OperationID] = index
	}
	rows, err := store.pool.Query(ctx, `
		SELECT timing.operation_id,assignment.generation,timing.stage,
		       timing.observed_at,timing.received_at,assignment.created_at
		FROM secondbox.assignment_stage_timings AS timing
		JOIN secondbox.assignments AS assignment ON assignment.id=timing.assignment_id
		WHERE timing.operation_id=ANY($1::text[])
		  AND timing.stage IN (`+startupTimingStagesSQL+`)
		ORDER BY timing.operation_id,assignment.generation,timing.observed_at,timing.stage`,
		operationIDs,
	)
	if err != nil {
		return fmt.Errorf("SecondBox boot timing list failed: %w", err)
	}
	defer rows.Close()
	type bootKey struct {
		operationID string
		generation  int64
	}
	bootIndexes := make(map[bootKey]int)
	previousObserved := make(map[bootKey]time.Time)
	for rows.Next() {
		var operationID, stage string
		var generation int64
		var observedAt, receivedAt, assignmentCreatedAt time.Time
		if err := rows.Scan(
			&operationID, &generation, &stage,
			&observedAt, &receivedAt, &assignmentCreatedAt,
		); err != nil {
			return fmt.Errorf("SecondBox boot timing scan failed: %w", err)
		}
		operationIndex, exists := operationIndexes[operationID]
		if !exists {
			return errors.New("SecondBox boot timing references an unselected Operation")
		}
		key := bootKey{operationID: operationID, generation: generation}
		bootIndex, exists := bootIndexes[key]
		if !exists {
			bootIndex = len(operations[operationIndex].Boots)
			bootIndexes[key] = bootIndex
			operations[operationIndex].Boots = append(
				operations[operationIndex].Boots,
				contracts.BootTiming{
					Generation: generation,
					Stages:     []contracts.BootStageTiming{},
				},
			)
			previousObserved[key] = assignmentCreatedAt
		}
		previous := previousObserved[key]
		elapsed := durationMillisecondsFloat(previous, observedAt)
		cumulative := durationMillisecondsFloat(assignmentCreatedAt, observedAt)
		boot := &operations[operationIndex].Boots[bootIndex]
		boot.Stages = append(boot.Stages, contracts.BootStageTiming{
			Stage: stage, ObservedAt: observedAt, ReceivedAt: receivedAt,
			ElapsedMilliseconds: elapsed, CumulativeMilliseconds: cumulative,
		})
		boot.DurationMilliseconds = cumulative
		if stage == "ready" {
			boot.Completed = true
		}
		previousObserved[key] = observedAt
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("SecondBox boot timing rows failed: %w", err)
	}
	return nil
}

func durationMillisecondsFloat(start, end time.Time) float64 {
	return max(float64(end.Sub(start))/float64(time.Millisecond), 0)
}

func (store *PostgresControlPlaneStore) readSandboxExecTimings(
	ctx context.Context,
	tenantRef string,
	subjectRef string,
	sandboxID string,
	limit int,
) ([]contracts.ExecTiming, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT id,operation,terminal_kind,elapsed_milliseconds,created_at,completed_at
		FROM secondbox.data_plane_sessions
		WHERE tenant_ref=$1 AND subject_ref=$2 AND sandbox_id=$3
		  AND kind='exec' AND operation IN ('exec','exec-stream')
		  AND terminal_kind IN ('exited','deadline_exceeded')
		  AND completed_at IS NOT NULL
		ORDER BY completed_at DESC,id DESC
		LIMIT $4`,
		tenantRef, subjectRef, sandboxID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("SecondBox Exec timing list failed: %w", err)
	}
	defer rows.Close()
	timings := make([]contracts.ExecTiming, 0)
	for rows.Next() {
		var timing contracts.ExecTiming
		var operation string
		if err := rows.Scan(
			&timing.SessionID, &operation, &timing.Outcome,
			&timing.ElapsedMilliseconds, &timing.CreatedAt, &timing.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("SecondBox Exec timing scan failed: %w", err)
		}
		switch operation {
		case "exec":
			timing.Mode = "buffered"
		case "exec-stream":
			timing.Mode = "streaming"
		default:
			return nil, errors.New("SecondBox Exec timing mode is invalid")
		}
		timings = append(timings, timing)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SecondBox Exec timing rows failed: %w", err)
	}
	return timings, nil
}

// ReadDeploymentTiming returns bounded aggregate database timing evidence.
func (store *PostgresControlPlaneStore) ReadDeploymentTiming(
	ctx context.Context,
	since time.Time,
	until time.Time,
) (contracts.DeploymentTimingSummary, error) {
	summary := contracts.DeploymentTimingSummary{
		BootStages: []contracts.BootStageTimingSummary{},
		ExecSeries: []contracts.ExecTimingSummary{},
		APISeries:  []contracts.HTTPRouteTimingSummary{},
		Operations: []contracts.OperationTimingSummary{},
	}
	if err := scanDurationPercentiles(
		store.pool.QueryRow(ctx, `
			SELECT count(*),
			       COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms),0),
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms),0),
			       COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms),0)
			FROM (
				SELECT GREATEST(
					EXTRACT(EPOCH FROM (timing.observed_at-assignment.created_at))*1000,
					0
				) AS duration_ms
				FROM secondbox.assignment_stage_timings AS timing
				JOIN secondbox.assignments AS assignment ON assignment.id=timing.assignment_id
				WHERE timing.stage='ready'
				  AND timing.observed_at >= $1 AND timing.observed_at <= $2
			) AS completed_boots`, since.UTC(), until.UTC()),
		&summary.Boot,
	); err != nil {
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment boot timing aggregate failed: %w", err,
		)
	}
	stageRows, err := store.pool.Query(ctx, `
		WITH eligible_assignments AS (
			SELECT assignment_id
			FROM secondbox.assignment_stage_timings
			WHERE stage='ready' AND observed_at >= $1 AND observed_at <= $2
		),
		ordered_stages AS (
			SELECT timing.stage,
			       GREATEST(
				       EXTRACT(EPOCH FROM (
					       timing.observed_at -
					       lag(timing.observed_at,1,assignment.created_at)
					       OVER (
						       PARTITION BY timing.assignment_id
						       ORDER BY timing.observed_at,timing.stage
					       )
				       ))*1000,
				       0
			       ) AS duration_ms
			FROM secondbox.assignment_stage_timings AS timing
			JOIN eligible_assignments AS eligible
			  ON eligible.assignment_id=timing.assignment_id
			JOIN secondbox.assignments AS assignment
			  ON assignment.id=timing.assignment_id
			WHERE timing.stage IN (`+startupTimingStagesSQL+`)
		)
		SELECT stage,count(*),
		       percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms),
		       percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms)
		FROM ordered_stages
		GROUP BY stage
		ORDER BY stage`, since.UTC(), until.UTC())
	if err != nil {
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment boot-stage timing aggregate failed: %w", err,
		)
	}
	for stageRows.Next() {
		var stage contracts.BootStageTimingSummary
		if err := scanNamedDurationPercentiles(stageRows, &stage.Stage, &stage.Duration); err != nil {
			stageRows.Close()
			return contracts.DeploymentTimingSummary{}, fmt.Errorf(
				"SecondBox deployment boot-stage timing scan failed: %w", err,
			)
		}
		summary.BootStages = append(summary.BootStages, stage)
	}
	if err := stageRows.Err(); err != nil {
		stageRows.Close()
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment boot-stage timing rows failed: %w", err,
		)
	}
	stageRows.Close()
	summary.DominantBootStage = dominantBootStage(summary.BootStages)

	if err := scanDurationPercentiles(
		store.pool.QueryRow(ctx, `
			SELECT count(*),
			       COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY elapsed_milliseconds),0),
			       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY elapsed_milliseconds),0),
			       COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY elapsed_milliseconds),0)
			FROM secondbox.data_plane_sessions
			WHERE kind='exec' AND operation IN ('exec','exec-stream')
			  AND terminal_kind IN ('exited','deadline_exceeded')
			  AND completed_at >= $1 AND completed_at <= $2`,
			since.UTC(), until.UTC()),
		&summary.Exec,
	); err != nil {
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment Exec timing aggregate failed: %w", err,
		)
	}
	execRows, err := store.pool.Query(ctx, `
		SELECT CASE operation WHEN 'exec' THEN 'buffered' ELSE 'streaming' END,
		       terminal_kind,count(*),
		       percentile_cont(0.50) WITHIN GROUP (ORDER BY elapsed_milliseconds),
		       percentile_cont(0.95) WITHIN GROUP (ORDER BY elapsed_milliseconds),
		       percentile_cont(0.99) WITHIN GROUP (ORDER BY elapsed_milliseconds)
		FROM secondbox.data_plane_sessions
		WHERE kind='exec' AND operation IN ('exec','exec-stream')
		  AND terminal_kind IN ('exited','deadline_exceeded')
		  AND completed_at >= $1 AND completed_at <= $2
		GROUP BY operation,terminal_kind
		ORDER BY operation,terminal_kind`, since.UTC(), until.UTC())
	if err != nil {
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment Exec timing-series aggregate failed: %w", err,
		)
	}
	for execRows.Next() {
		var series contracts.ExecTimingSummary
		if err := scanTwoNamedDurationPercentiles(
			execRows, &series.Mode, &series.Outcome, &series.Duration,
		); err != nil {
			execRows.Close()
			return contracts.DeploymentTimingSummary{}, fmt.Errorf(
				"SecondBox deployment Exec timing-series scan failed: %w", err,
			)
		}
		summary.ExecSeries = append(summary.ExecSeries, series)
	}
	if err := execRows.Err(); err != nil {
		execRows.Close()
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment Exec timing-series rows failed: %w", err,
		)
	}
	execRows.Close()

	operationRows, err := store.pool.Query(ctx, `
		SELECT kind,state,
		       count(started_at),
		       COALESCE(percentile_cont(0.50) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (started_at-created_at))*1000
		       ) FILTER (WHERE started_at IS NOT NULL),0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (started_at-created_at))*1000
		       ) FILTER (WHERE started_at IS NOT NULL),0),
		       COALESCE(percentile_cont(0.99) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (started_at-created_at))*1000
		       ) FILTER (WHERE started_at IS NOT NULL),0),
		       count(*) FILTER (WHERE started_at IS NOT NULL AND completed_at IS NOT NULL),
		       COALESCE(percentile_cont(0.50) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (completed_at-started_at))*1000
		       ) FILTER (WHERE started_at IS NOT NULL AND completed_at IS NOT NULL),0),
		       COALESCE(percentile_cont(0.95) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (completed_at-started_at))*1000
		       ) FILTER (WHERE started_at IS NOT NULL AND completed_at IS NOT NULL),0),
		       COALESCE(percentile_cont(0.99) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (completed_at-started_at))*1000
		       ) FILTER (WHERE started_at IS NOT NULL AND completed_at IS NOT NULL),0),
		       count(*),
		       percentile_cont(0.50) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (completed_at-created_at))*1000
		       ),
		       percentile_cont(0.95) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (completed_at-created_at))*1000
		       ),
		       percentile_cont(0.99) WITHIN GROUP (
			       ORDER BY EXTRACT(EPOCH FROM (completed_at-created_at))*1000
		       )
		FROM secondbox.operations
		WHERE completed_at >= $1 AND completed_at <= $2
		  AND state IN ('succeeded','failed','cancelled')
		  AND kind IN (
		      'create','start','drain','stop','delete',
		      'snapshot_create','snapshot_delete','snapshot_restore',
		      'cancel_exec','cancel_terminal'
		  )
		GROUP BY kind,state
		ORDER BY kind,state`, since.UTC(), until.UTC())
	if err != nil {
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment Operation timing aggregate failed: %w", err,
		)
	}
	for operationRows.Next() {
		var series contracts.OperationTimingSummary
		var queueCount, executionCount, totalCount int64
		var queueP50, queueP95, queueP99 float64
		var executionP50, executionP95, executionP99 float64
		var totalP50, totalP95, totalP99 float64
		if err := operationRows.Scan(
			&series.Kind, &series.State,
			&queueCount, &queueP50, &queueP95, &queueP99,
			&executionCount, &executionP50, &executionP95, &executionP99,
			&totalCount, &totalP50, &totalP95, &totalP99,
		); err != nil {
			operationRows.Close()
			return contracts.DeploymentTimingSummary{}, fmt.Errorf(
				"SecondBox deployment Operation timing scan failed: %w", err,
			)
		}
		series.Queue = durationPercentiles(queueCount, queueP50, queueP95, queueP99)
		series.Execution = durationPercentiles(
			executionCount, executionP50, executionP95, executionP99,
		)
		series.Total = durationPercentiles(totalCount, totalP50, totalP95, totalP99)
		summary.Operations = append(summary.Operations, series)
	}
	if err := operationRows.Err(); err != nil {
		operationRows.Close()
		return contracts.DeploymentTimingSummary{}, fmt.Errorf(
			"SecondBox deployment Operation timing rows failed: %w", err,
		)
	}
	operationRows.Close()
	return summary, nil
}

func scanDurationPercentiles(
	row pgx.Row,
	destination *contracts.DurationPercentiles,
) error {
	var count int64
	var p50, p95, p99 float64
	if err := row.Scan(&count, &p50, &p95, &p99); err != nil {
		return err
	}
	*destination = durationPercentiles(count, p50, p95, p99)
	return nil
}

func scanNamedDurationPercentiles(
	row rowScanner,
	name *string,
	destination *contracts.DurationPercentiles,
) error {
	var count int64
	var p50, p95, p99 float64
	if err := row.Scan(name, &count, &p50, &p95, &p99); err != nil {
		return err
	}
	*destination = durationPercentiles(count, p50, p95, p99)
	return nil
}

func scanTwoNamedDurationPercentiles(
	row rowScanner,
	first *string,
	second *string,
	destination *contracts.DurationPercentiles,
) error {
	var count int64
	var p50, p95, p99 float64
	if err := row.Scan(first, second, &count, &p50, &p95, &p99); err != nil {
		return err
	}
	*destination = durationPercentiles(count, p50, p95, p99)
	return nil
}

func durationPercentiles(
	count int64,
	p50 float64,
	p95 float64,
	p99 float64,
) contracts.DurationPercentiles {
	summary := contracts.DurationPercentiles{Count: count}
	if count == 0 {
		return summary
	}
	p50Milliseconds := max(p50, 0)
	p95Milliseconds := max(p95, 0)
	p99Milliseconds := max(p99, 0)
	summary.P50Milliseconds = &p50Milliseconds
	summary.P95Milliseconds = &p95Milliseconds
	summary.P99Milliseconds = &p99Milliseconds
	return summary
}

func dominantBootStage(
	stages []contracts.BootStageTimingSummary,
) *contracts.BootStageTimingSummary {
	candidates := append([]contracts.BootStageTimingSummary(nil), stages...)
	sort.Slice(candidates, func(left, right int) bool {
		leftP95 := float64(-1)
		if candidates[left].Duration.P95Milliseconds != nil {
			leftP95 = *candidates[left].Duration.P95Milliseconds
		}
		rightP95 := float64(-1)
		if candidates[right].Duration.P95Milliseconds != nil {
			rightP95 = *candidates[right].Duration.P95Milliseconds
		}
		if leftP95 == rightP95 {
			return candidates[left].Stage < candidates[right].Stage
		}
		return leftP95 > rightP95
	})
	if len(candidates) == 0 || candidates[0].Duration.P95Milliseconds == nil {
		return nil
	}
	dominant := candidates[0]
	return &dominant
}

func (store *PostgresControlPlaneStore) readTimingMetrics(
	ctx context.Context,
	snapshot *contracts.MetricsSnapshot,
) error {
	snapshot.BootStageDurations = []contracts.BootStageDurationMetric{}
	snapshot.ExecDurations = []contracts.ExecDurationMetric{}
	bootQuery := `
		SELECT ` + durationHistogramProjection("duration_milliseconds") + `
		FROM (
			SELECT GREATEST(
				EXTRACT(EPOCH FROM (timing.observed_at-assignment.created_at))*1000,
				0
			) AS duration_milliseconds
			FROM secondbox.assignment_stage_timings AS timing
			JOIN secondbox.assignments AS assignment ON assignment.id=timing.assignment_id
			WHERE timing.stage='ready'
		) AS completed_boots`
	if err := scanMetricDurationHistogram(
		store.pool.QueryRow(ctx, bootQuery), &snapshot.BootDuration,
	); err != nil {
		return fmt.Errorf("SecondBox boot duration metrics projection failed: %w", err)
	}

	stageQuery := `
		WITH eligible_assignments AS (
			SELECT assignment_id
			FROM secondbox.assignment_stage_timings
			WHERE stage='ready'
		),
		ordered_stages AS (
			SELECT timing.stage,
			       GREATEST(
				       EXTRACT(EPOCH FROM (
					       timing.observed_at -
					       lag(timing.observed_at,1,assignment.created_at)
					       OVER (
						       PARTITION BY timing.assignment_id
						       ORDER BY timing.observed_at,timing.stage
					       )
				       ))*1000,
				       0
			       ) AS duration_milliseconds
			FROM secondbox.assignment_stage_timings AS timing
			JOIN eligible_assignments AS eligible
			  ON eligible.assignment_id=timing.assignment_id
			JOIN secondbox.assignments AS assignment
			  ON assignment.id=timing.assignment_id
			WHERE timing.stage IN (` + startupTimingStagesSQL + `)
		)
		SELECT stage,` + durationHistogramProjection("duration_milliseconds") + `
		FROM ordered_stages
		GROUP BY stage
		ORDER BY stage`
	stageRows, err := store.pool.Query(ctx, stageQuery)
	if err != nil {
		return fmt.Errorf("SecondBox boot-stage duration metrics projection failed: %w", err)
	}
	for stageRows.Next() {
		var metric contracts.BootStageDurationMetric
		if err := scanNamedMetricDurationHistogram(
			stageRows, &metric.Stage, &metric.Histogram,
		); err != nil {
			stageRows.Close()
			return fmt.Errorf("SecondBox boot-stage duration metrics scan failed: %w", err)
		}
		snapshot.BootStageDurations = append(snapshot.BootStageDurations, metric)
	}
	if err := stageRows.Err(); err != nil {
		stageRows.Close()
		return fmt.Errorf("SecondBox boot-stage duration metrics rows failed: %w", err)
	}
	stageRows.Close()

	execQuery := `
		SELECT CASE operation WHEN 'exec' THEN 'buffered' ELSE 'streaming' END,
		       terminal_kind,` + durationHistogramProjection("elapsed_milliseconds") + `
		FROM secondbox.data_plane_sessions
		WHERE kind='exec' AND operation IN ('exec','exec-stream')
		  AND terminal_kind IN ('exited','deadline_exceeded')
		  AND completed_at IS NOT NULL
		GROUP BY operation,terminal_kind
		ORDER BY operation,terminal_kind`
	execRows, err := store.pool.Query(ctx, execQuery)
	if err != nil {
		return fmt.Errorf("SecondBox Exec duration metrics projection failed: %w", err)
	}
	for execRows.Next() {
		var metric contracts.ExecDurationMetric
		if err := scanTwoNamedMetricDurationHistogram(
			execRows, &metric.Mode, &metric.Outcome, &metric.Histogram,
		); err != nil {
			execRows.Close()
			return fmt.Errorf("SecondBox Exec duration metrics scan failed: %w", err)
		}
		snapshot.ExecDurations = append(snapshot.ExecDurations, metric)
	}
	if err := execRows.Err(); err != nil {
		execRows.Close()
		return fmt.Errorf("SecondBox Exec duration metrics rows failed: %w", err)
	}
	execRows.Close()
	return nil
}

func durationHistogramProjection(millisecondsExpression string) string {
	var projection strings.Builder
	projection.WriteString("count(*),COALESCE(sum((")
	projection.WriteString(millisecondsExpression)
	projection.WriteString(")/1000.0),0)")
	for _, bucketSeconds := range observability.DurationBucketsSeconds {
		projection.WriteString(",count(*) FILTER (WHERE ")
		projection.WriteString(millisecondsExpression)
		projection.WriteString("<=")
		projection.WriteString(strconv.FormatFloat(bucketSeconds*1000, 'f', -1, 64))
		projection.WriteString(")")
	}
	return projection.String()
}

func scanMetricDurationHistogram(
	row rowScanner,
	destination *contracts.MetricDurationHistogram,
) error {
	var bucketCounts [len(observability.DurationBucketsSeconds)]uint64
	destinations := []any{&destination.Count, &destination.SumSeconds}
	for index := range bucketCounts {
		destinations = append(destinations, &bucketCounts[index])
	}
	if err := row.Scan(destinations...); err != nil {
		return err
	}
	destination.BucketCounts = append([]uint64(nil), bucketCounts[:]...)
	return nil
}

func scanNamedMetricDurationHistogram(
	row rowScanner,
	name *string,
	destination *contracts.MetricDurationHistogram,
) error {
	var bucketCounts [len(observability.DurationBucketsSeconds)]uint64
	destinations := []any{name, &destination.Count, &destination.SumSeconds}
	for index := range bucketCounts {
		destinations = append(destinations, &bucketCounts[index])
	}
	if err := row.Scan(destinations...); err != nil {
		return err
	}
	destination.BucketCounts = append([]uint64(nil), bucketCounts[:]...)
	return nil
}

func scanTwoNamedMetricDurationHistogram(
	row rowScanner,
	first *string,
	second *string,
	destination *contracts.MetricDurationHistogram,
) error {
	var bucketCounts [len(observability.DurationBucketsSeconds)]uint64
	destinations := []any{first, second, &destination.Count, &destination.SumSeconds}
	for index := range bucketCounts {
		destinations = append(destinations, &bucketCounts[index])
	}
	if err := row.Scan(destinations...); err != nil {
		return err
	}
	destination.BucketCounts = append([]uint64(nil), bucketCounts[:]...)
	return nil
}

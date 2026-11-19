package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
)

// The Runner's reported warning/denial already applies its configured physical
// thresholds and recovery hysteresis. One transfer per Runner bounds staging IO.
func (store *PostgresControlPlaneStore) archivePressureWorkspaces(ctx context.Context, now time.Time, profiles map[string]int64) error {
	for profile, idleSeconds := range profiles {
		if idleSeconds < 1 {
			return errors.New("SecondBox archive pressure policy requires a positive cold idle duration")
		}
		rows, err := store.pool.Query(ctx, `SELECT DISTINCT ON (runner.id)
 sandbox.id,sandbox.tenant_ref,sandbox.subject_ref,sandbox.revision
 FROM secondbox.sandboxes sandbox
 JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id
 JOIN secondbox.runners runner ON runner.id=workspace.home_runner_id
 WHERE sandbox.profile_name=$1 AND sandbox.state='stopped' AND sandbox.desired_state='stopped' AND sandbox.current_instance_id=''
 AND workspace.state='ready' AND workspace.mutation_state=''
 AND runner.state IN ('ready','draining') AND runner.active_connection_id<>''
 AND runner.capabilities_json ? $2 AND runner.storage_pressure_json->>'status' IN ('warning','admission_denied')
 AND NOT EXISTS(SELECT 1 FROM secondbox.snapshots snapshot WHERE snapshot.workspace_id=workspace.id AND snapshot.state<>'deleted')
 AND NOT EXISTS(SELECT 1 FROM secondbox.assignments assignment WHERE assignment.sandbox_id=sandbox.id AND assignment.state NOT IN ('fenced','released','failed_terminal'))
 AND NOT EXISTS(SELECT 1 FROM secondbox.workspace_archives archive WHERE archive.source_runner_id=runner.id AND archive.state NOT IN ('succeeded','failed'))
 ORDER BY runner.id,sandbox.updated_at,sandbox.id LIMIT 16`, profile, contracts.RunnerCapabilityPhysicalStorageAdmission)
		if err != nil {
			return fmt.Errorf("SecondBox archive pressure candidates: %w", err)
		}
		var candidates []ports.WorkspaceArchiveInput
		for rows.Next() {
			var input ports.WorkspaceArchiveInput
			if err := rows.Scan(&input.SandboxID, &input.Principal.TenantRef, &input.Principal.SubjectRef, &input.ExpectedRevision); err != nil {
				rows.Close()
				return err
			}
			input.ArchiveID, err = newWorkspaceArchiveID()
			if err != nil {
				rows.Close()
				return err
			}
			input.FencingToken = make([]byte, 32)
			if _, err := rand.Read(input.FencingToken); err != nil {
				rows.Close()
				return err
			}
			input.CommandID, input.IdempotencyKey, input.RequestHash = input.ArchiveID, input.ArchiveID, input.ArchiveID
			input.Now, input.IdempotencyEnds = now, now.Add(24*time.Hour)
			input.Operation = contracts.Operation{ID: input.ArchiveID, SandboxID: input.SandboxID, Kind: "archive", State: "pending", RequestID: input.ArchiveID, CreatedAt: now, UpdatedAt: now}
			input.AuditEvent = contracts.AuditEvent{ID: input.ArchiveID, TenantRef: input.Principal.TenantRef, SubjectRef: input.Principal.SubjectRef, ActorKind: "system", ActorID: "workspace-pressure-policy", Action: "sandbox.archive", ResourceKind: "sandbox", ResourceID: input.SandboxID, Outcome: "accepted", RequestID: input.ArchiveID, Details: map[string]string{"trigger": "storage_pressure"}, CreatedAt: now}
			candidates = append(candidates, input)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			if _, err := store.ArchiveSandbox(ctx, candidate); err != nil && !errors.Is(err, ports.ErrWorkspaceMutation) && !errors.Is(err, ports.ErrRevisionConflict) && !errors.Is(err, ports.ErrHomeRunnerUnavailable) && !errors.Is(err, ports.ErrSandboxNotStopped) && !errors.Is(err, ports.ErrRelocationSnapshotsPresent) {
				return fmt.Errorf("SecondBox pressure archive %s: %w", candidate.SandboxID, err)
			}
		}
	}
	return nil
}

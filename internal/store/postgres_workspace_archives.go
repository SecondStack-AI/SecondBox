package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	"github.com/SecondStack-AI/SecondBox/internal/workspacearchive"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"google.golang.org/protobuf/proto"
)

// ConfigureWorkspaceArchives fixes archive authority and Profile policy before requests or workers start.
func (store *PostgresControlPlaneStore) ConfigureWorkspaceArchives(ctx context.Context, provider workspacearchive.TransferProvider, profileIdleSeconds map[string]int64) error {
	if store.archiveConfigured {
		return errors.New("SecondBox workspace archive configuration is immutable until restart")
	}
	for profile, seconds := range profileIdleSeconds {
		if provider == nil || profile == "" || seconds < 1 || seconds > 365*24*3600 {
			return errors.New("SecondBox workspace archive policy requires a transfer provider, Profile and positive idle duration no greater than one year")
		}
	}
	if provider == nil {
		var required bool
		if err := store.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM secondbox.workspaces WHERE state IN ('archived','restoring') OR mutation_kind IN ('archive','archive_restore'))
 OR EXISTS(SELECT 1 FROM secondbox.workspace_archives WHERE state NOT IN ('succeeded','failed'))
 OR EXISTS(SELECT 1 FROM secondbox.archive_cleanup)`).Scan(&required); err != nil {
			return fmt.Errorf("SecondBox workspace archive configuration state check: %w", err)
		}
		if required {
			return errors.New("SecondBox workspace archive configuration is required until all archived Workspaces are restored and archive mutations, transfers and object cleanup finish")
		}
	}
	policy := make(map[string]int64, len(profileIdleSeconds))
	for profile, seconds := range profileIdleSeconds {
		policy[profile] = seconds
	}
	store.archiveTransfers = provider
	store.archiveProfileIdleSeconds = policy
	store.archiveConfigured = true
	return nil
}

func (store *PostgresControlPlaneStore) ArchiveSandbox(ctx context.Context, input ports.WorkspaceArchiveInput) (contracts.Operation, error) {
	if store.archiveTransfers == nil {
		return contracts.Operation{}, errors.New("SecondBox workspace archive tier is not configured")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return contracts.Operation{}, err
	}
	defer tx.Rollback(ctx)
	identity := ports.AdminIdempotencyInput{TenantRef: input.Principal.TenantRef, SubjectRef: input.Principal.SubjectRef, Operation: "sandbox.archive", TargetID: input.SandboxID, Key: input.IdempotencyKey, RequestHash: input.RequestHash, Now: input.Now, Ends: input.IdempotencyEnds, AuditEvent: &input.AuditEvent}
	var previous contracts.Operation
	if _, found, err := lookupAdminIdempotency(ctx, tx, identity, &previous); err != nil {
		return contracts.Operation{}, err
	} else if found {
		return previous, tx.Commit(ctx)
	}
	locked, err := rowlock.SandboxWorkspaceForSubject(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.SandboxID)
	if err != nil {
		return contracts.Operation{}, mapNotFound(err, ports.ErrSandboxNotFound)
	}
	if locked.Revision != input.ExpectedRevision {
		return contracts.Operation{}, ports.ErrRevisionConflict
	}
	if locked.SandboxState != "stopped" || locked.DesiredState != "stopped" || locked.CurrentInstanceID != "" {
		return contracts.Operation{}, ports.ErrSandboxNotStopped
	}
	workspace := locked.Workspace
	if workspace.State != "ready" || workspace.Generation != locked.Generation {
		return contracts.Operation{}, ports.ErrGenerationFenced
	}
	if workspace.Mutation.State != "" {
		return contracts.Operation{}, ports.ErrWorkspaceMutation
	}
	input.Operation.SandboxID = input.SandboxID
	var retained bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secondbox.snapshots WHERE workspace_id=$1 AND state<>'deleted')`, workspace.ID).Scan(&retained); err != nil {
		return contracts.Operation{}, err
	}
	if retained {
		return contracts.Operation{}, ports.ErrRelocationSnapshotsPresent
	}
	var sourceAvailable bool
	if err := tx.QueryRow(ctx, `SELECT state IN ('ready','draining') AND active_connection_id<>'' FROM secondbox.runners WHERE id=$1 FOR SHARE`, workspace.HomeRunnerID).Scan(&sourceAvailable); err != nil {
		return contracts.Operation{}, err
	}
	if !sourceAvailable {
		return contracts.Operation{}, ports.ErrHomeRunnerUnavailable
	}
	key := fmt.Sprintf("workspaces/%s/%d/%s.sbx.gz", workspace.ID, workspace.Generation, input.ArchiveID)
	if err := setWorkspaceMutation(ctx, tx, workspace.ID, "archive", input.ArchiveID, input.CommandID, input.Operation.ID, workspace.Generation, workspace.Generation, input.Now); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.workspace_archives(id,operation_id,sandbox_id,workspace_id,source_runner_id,generation,logical_capacity_bytes,archive_key,state,command_id,fencing_token,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'queued',$9,$10,$11,$11)`, input.ArchiveID, input.Operation.ID, input.SandboxID, workspace.ID, workspace.HomeRunnerID, workspace.Generation, workspace.LogicalCapacityBytes, key, input.CommandID, input.FencingToken, input.Now); err != nil {
		return contracts.Operation{}, err
	}
	if err := insertOperation(ctx, tx, input.Principal.TenantRef, input.Principal.SubjectRef, input.Operation); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := insertAdminIdempotency(ctx, tx, identity, input.Operation); err != nil {
		return contracts.Operation{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET revision=revision+1,updated_at=$2 WHERE id=$1`, input.SandboxID, input.Now); err != nil {
		return contracts.Operation{}, err
	}
	return input.Operation, tx.Commit(ctx)
}

type workspaceArchiveCommand struct {
	id, operationID, sandboxID, workspaceID, source, target, key, state, commandID string
	generation, capacity                                                           int64
	fence                                                                          []byte
	descriptorJSON                                                                 []byte
	commandPayload                                                                 []byte
}

// ReconcileWorkspaceArchives moves only metadata through the control plane.
// The Runner exchanges its sealed disk with the object store directly.
func (store *PostgresControlPlaneStore) ReconcileWorkspaceArchives(ctx context.Context, now time.Time, profileIdleSeconds map[string]int64) error {
	if store.archiveTransfers == nil {
		return nil
	}
	var failures []error
	record := func(stage string, err error) {
		if err != nil {
			failures = append(failures, fmt.Errorf("SecondBox archive reconciliation %s: %w", stage, err))
		}
	}
	record("expiry", store.expireArchiveStartWaits(ctx, now))
	record("lost restore recovery", store.recoverLostWorkspaceRestores(ctx, now))
	record("resident starts", store.prepareResidentWorkspaceStarts(ctx, now, profileIdleSeconds))
	record("unhomed creates", store.prepareUnhomedWorkspaceCreates(ctx, now))
	record("archived starts", store.prepareArchivedWorkspaceStarts(ctx, now))
	record("archived deletion", store.deleteArchivedWorkspaces(ctx, now))
	record("idle policy", store.archiveIdleWorkspaces(ctx, now, profileIdleSeconds))
	record("pressure policy", store.archivePressureWorkspaces(ctx, now, profileIdleSeconds))
	record("commands", store.queuePendingWorkspaceArchiveCommands(ctx, now))
	record("object cleanup", store.collectArchiveObjects(ctx, now))
	return errors.Join(failures...)
}

func (store *PostgresControlPlaneStore) queuePendingWorkspaceArchiveCommands(ctx context.Context, now time.Time) error {
	rows, err := store.pool.Query(ctx, `SELECT archive.id,archive.operation_id,archive.sandbox_id,archive.workspace_id,archive.source_runner_id,archive.target_runner_id,archive.generation,archive.logical_capacity_bytes,archive.archive_key,archive.state,archive.command_id,archive.fencing_token,archive.descriptor_json,command.payload FROM secondbox.workspace_archives archive LEFT JOIN secondbox.runner_commands command ON command.id=archive.command_id WHERE (archive.state IN ('queued','restore_queued','cleanup_queued','abort_queued') OR (archive.state IN ('queued_sent','restore_queued_sent') AND command.state='pending')) AND (archive.not_before IS NULL OR archive.not_before<=$1) ORDER BY archive.updated_at,archive.id LIMIT 32`, now)
	if err != nil {
		return err
	}
	var commands []workspaceArchiveCommand
	for rows.Next() {
		var command workspaceArchiveCommand
		if err := rows.Scan(&command.id, &command.operationID, &command.sandboxID, &command.workspaceID, &command.source, &command.target, &command.generation, &command.capacity, &command.key, &command.state, &command.commandID, &command.fence, &command.descriptorJSON, &command.commandPayload); err != nil {
			rows.Close()
			return err
		}
		commands = append(commands, command)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var failures []error
	for _, command := range commands {
		var commandErr error
		if strings.HasSuffix(command.state, "_sent") {
			envelope := new(runnerv1.ControlPlaneToRunner)
			commandErr = proto.Unmarshal(command.commandPayload, envelope)
			if commandErr == nil {
				if grant := envelope.GetLocalWorkspace().GetArchiveTransfer(); grant != nil && grant.ExpiresAtUnixMs > uint64(now.Add(time.Minute).UnixMilli()) {
					continue
				}
			}
		}
		if commandErr == nil {
			commandErr = store.queueWorkspaceArchiveCommand(ctx, command, now)
		}
		if commandErr != nil {
			failures = append(failures, fmt.Errorf("SecondBox archive command %s: %w", command.id, commandErr))
			if _, err := store.pool.Exec(ctx, `UPDATE secondbox.workspace_archives SET not_before=$2,updated_at=$3 WHERE id=$1 AND state=$4 AND command_id=$5`, command.id, now.Add(workspacearchive.ReconciliationRetryDelay), now, command.state, command.commandID); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

func (store *PostgresControlPlaneStore) queueWorkspaceArchiveCommand(ctx context.Context, record workspaceArchiveCommand, now time.Time) error {
	kind := runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_EXPORT
	runnerID := record.source
	var transfer *runnerv1.WorkspaceArchiveTransfer
	var err error
	phase := strings.TrimSuffix(record.state, "_sent")
	switch phase {
	case "queued":
		transfer, err = store.archiveTransfers.Transfer(ctx, record.key, http.MethodPut, &runnerv1.WorkspaceArchiveDescriptor{ArchiveBytes: uint64(record.capacity) + uint64(record.capacity)/8 + 1<<20})
	case "restore_queued":
		kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT
		runnerID = record.target
		var descriptor runnerv1.WorkspaceArchiveDescriptor
		if err = json.Unmarshal(record.descriptorJSON, &descriptor); err == nil {
			transfer, err = store.archiveTransfers.Transfer(ctx, record.key, http.MethodGet, &descriptor)
		}
	case "cleanup_queued":
		kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_DELETE_SOURCE
	case "abort_queued":
		kind = runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_RELOCATION_ABORT_SOURCE
	default:
		return errors.New("SecondBox archive command phase is invalid")
	}
	if err != nil {
		return err
	}
	command := localWorkspaceCommand(record.commandID, kind, contracts.Operation{ID: record.id, RequestID: record.operationID}, record.commandID, record.sandboxID, record.workspaceID, "", record.generation, record.generation, record.capacity, runnerID, record.fence)
	command.Correlation.OperationId = record.operationID
	command.ArchiveTransfer = transfer
	payload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{LocalWorkspace: command}})
	if err != nil {
		return err
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state, commandID string
	var notBefore *time.Time
	if err := tx.QueryRow(ctx, `SELECT state,command_id,not_before FROM secondbox.workspace_archives WHERE id=$1 FOR UPDATE`, record.id).Scan(&state, &commandID, &notBefore); err != nil {
		return err
	}
	if state != record.state || commandID != record.commandID || notBefore != nil && notBefore.After(now) {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.runner_commands(id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at) VALUES($1,$2,$3,'local-workspace',$4,'pending','',0,$5,$5) ON CONFLICT(id) DO UPDATE SET payload=EXCLUDED.payload,updated_at=EXCLUDED.updated_at WHERE secondbox.runner_commands.state='pending'`, record.commandID, runnerID, record.id, payload, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspace_archives SET state=$2,not_before=NULL,updated_at=$3 WHERE id=$1`, record.id, phase+"_sent", now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *PostgresControlPlaneStore) prepareArchivedWorkspaceStarts(ctx context.Context, now time.Time) error {
	rows, err := store.pool.Query(ctx, `SELECT sandbox.id FROM secondbox.sandboxes sandbox JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id WHERE sandbox.desired_state='running' AND sandbox.current_instance_id='' AND workspace.state='archived' AND workspace.mutation_kind='start' AND workspace.mutation_state='queued' ORDER BY sandbox.id LIMIT 32`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var failures []error
	for _, id := range ids {
		if err := store.prepareArchivedWorkspaceStart(ctx, id, now); err != nil && !errors.Is(err, ports.ErrRelocationTargetUnavailable) && !errors.Is(err, ports.ErrEgressContextUnavailable) && !errors.Is(err, ports.ErrWorkspaceMutation) {
			failures = append(failures, fmt.Errorf("SecondBox archived Workspace start %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (store *PostgresControlPlaneStore) prepareArchivedWorkspaceStart(ctx context.Context, sandboxID string, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return err
	}
	if locked.Workspace.State != "archived" || locked.DesiredState != "running" || locked.Workspace.Mutation.Kind != "start" {
		return ports.ErrWorkspaceMutation
	}
	var deadline *time.Time
	if err := tx.QueryRow(ctx, `SELECT capacity_wait_deadline FROM secondbox.operations WHERE id=$1`, locked.Workspace.Mutation.OperationID).Scan(&deadline); err != nil {
		return err
	}
	if deadline != nil && !deadline.After(now) {
		if _, err := tx.Exec(ctx, `UPDATE secondbox.operations SET state='failed',error_code='capacity_wait_expired',error_message='workspace restore capacity wait expired before placement',retryable=true,completed_at=$2,updated_at=$2 WHERE id=$1 AND state IN ('pending','running')`, locked.Workspace.Mutation.OperationID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET mutation_kind='',mutation_id='',mutation_effect_id='',mutation_operation_id='',mutation_state='',mutation_expected_generation=NULL,mutation_target_generation=NULL,updated_at=$2 WHERE id=$1`, locked.WorkspaceID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE secondbox.sandboxes SET desired_state='stopped',revision=revision+1,next_reconcile_at=NULL,updated_at=$2 WHERE id=$1`, sandboxID, now); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var specJSON, descriptorJSON []byte
	var key string
	if err := tx.QueryRow(ctx, `SELECT revision.spec_json,workspace.archive_key,workspace.archive_descriptor_json FROM secondbox.profile_revisions revision,secondbox.workspaces workspace WHERE revision.id=$1 AND workspace.id=$2`, locked.ProfileRevisionID, locked.WorkspaceID).Scan(&specJSON, &key, &descriptorJSON); err != nil {
		return err
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return err
	}
	spec = sandboxPlacementSpec(spec, locked.Resources)
	// A former home is eligible again only after its exact source deletion
	// succeeded. This also fences older copies across repeated transfers.
	rows, err := tx.Query(ctx, `SELECT DISTINCT archive.source_runner_id FROM secondbox.workspace_archives archive WHERE archive.workspace_id=$1 AND archive.source_runner_id<>'' AND archive.descriptor_json IS NOT NULL AND NOT(archive.state='succeeded' AND EXISTS(SELECT 1 FROM secondbox.runner_commands command WHERE command.id=archive.command_id AND command.assignment_id=archive.id AND command.runner_id=archive.source_runner_id AND command.kind='local-workspace' AND command.state='acknowledged'))`, locked.WorkspaceID)
	if err != nil {
		return err
	}
	var excludedHomes []string
	for rows.Next() {
		var home string
		if err := rows.Scan(&home); err != nil {
			rows.Close()
			return err
		}
		excludedHomes = append(excludedHomes, home)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	target, err := selectRunnerForPlacement(ctx, tx, spec, runnerPlacementOptions{
		excludedRunnerIDs:        excludedHomes,
		requireWorkspaceTransfer: true,
		unavailable:              ports.ErrRelocationTargetUnavailable,
		errorPrefix:              "SecondBox Workspace archive restore target",
		requiredEgressContext:    locked.EgressContext,
	})
	if err != nil {
		return err
	}
	identifier, err := newWorkspaceArchiveID()
	if err != nil {
		return err
	}
	fence := make([]byte, 32)
	if _, err := rand.Read(fence); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO secondbox.workspace_archives(id,operation_id,sandbox_id,workspace_id,source_runner_id,target_runner_id,generation,logical_capacity_bytes,archive_key,descriptor_json,state,command_id,fencing_token,created_at,updated_at) VALUES($1,$2,$3,$4,'',$5,$6,$7,$8,$9,'restore_queued',$1,$10,$11,$11)`, identifier, locked.Workspace.Mutation.OperationID, sandboxID, locked.WorkspaceID, target, locked.Generation, locked.Workspace.LogicalCapacityBytes, key, descriptorJSON, fence, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET state='restoring',mutation_kind='archive_restore',mutation_id=$2,mutation_effect_id=$2,mutation_state='queued',updated_at=$3 WHERE id=$1`, locked.WorkspaceID, identifier, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func newWorkspaceArchiveID() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return "archive_" + hex.EncodeToString(value[:]), nil
}

func (store *PostgresControlPlaneStore) prepareUnhomedWorkspaceCreates(ctx context.Context, now time.Time) error {
	rows, err := store.pool.Query(ctx, `SELECT sandbox.id FROM secondbox.sandboxes sandbox JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id WHERE workspace.home_runner_id='' AND workspace.state='creating' AND workspace.mutation_kind='create' AND workspace.mutation_state='queued' ORDER BY sandbox.id LIMIT 32`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var failures []error
	for _, id := range ids {
		if err := store.placeUnhomedWorkspaceCreate(ctx, id, now); err != nil && !errors.Is(err, ports.ErrHomeRunnerUnavailable) && !errors.Is(err, ports.ErrEgressContextUnavailable) && !errors.Is(err, ports.ErrWorkspaceMutation) {
			failures = append(failures, fmt.Errorf("SecondBox unhomed Workspace placement %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (store *PostgresControlPlaneStore) placeUnhomedWorkspaceCreate(ctx context.Context, sandboxID string, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	locked, err := rowlock.SandboxWorkspaceByID(ctx, tx, sandboxID)
	if err != nil {
		return err
	}
	if locked.Workspace.State != "creating" || locked.Workspace.HomeRunnerID != "" || locked.Workspace.Mutation.Kind != "create" {
		return ports.ErrWorkspaceMutation
	}
	var deadline *time.Time
	if err := tx.QueryRow(ctx, `SELECT capacity_wait_deadline FROM secondbox.operations WHERE id=$1`, locked.Workspace.Mutation.OperationID).Scan(&deadline); err != nil {
		return err
	}
	if locked.DesiredState == "deleted" || deadline != nil && !deadline.After(now) {
		state := "failed"
		code := "capacity_wait_expired"
		message := "workspace capacity wait expired before a Runner became available"
		if locked.DesiredState == "deleted" {
			state = "deleted"
			code = "create_cancelled"
			message = "workspace creation was cancelled before placement"
		}
		if err := finishUnhomedWorkspaceCreate(ctx, tx, locked, state, code, message, now); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	var specJSON []byte
	if err := tx.QueryRow(ctx, `SELECT spec_json FROM secondbox.profile_revisions WHERE id=$1`, locked.ProfileRevisionID).Scan(&specJSON); err != nil {
		return err
	}
	var spec contracts.ProfileRevisionSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return err
	}
	spec = sandboxPlacementSpec(spec, locked.Resources)
	home, err := selectInitialHomeRunner(ctx, tx, spec, locked.EgressContext)
	if err != nil {
		return err
	}
	var commandID string
	var payload []byte
	if err := tx.QueryRow(ctx, `SELECT id,payload FROM secondbox.runner_commands WHERE assignment_id=$1 AND runner_id='' AND kind='local-workspace' FOR UPDATE`, locked.Workspace.Mutation.EffectID).Scan(&commandID, &payload); err != nil {
		return err
	}
	envelope := new(runnerv1.ControlPlaneToRunner)
	if err := proto.Unmarshal(payload, envelope); err != nil {
		return err
	}
	command := envelope.GetLocalWorkspace()
	if command == nil || command.Correlation == nil {
		return errors.New("SecondBox unhomed Workspace create command is invalid")
	}
	command.Correlation.RunnerId = home
	payload, err = proto.Marshal(envelope)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.workspaces SET home_runner_id=$2,updated_at=$3 WHERE id=$1`, locked.WorkspaceID, home, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.lifecycle_effects SET runner_id=$2,effect_deadline=$3,updated_at=$4 WHERE id=$1`, locked.Workspace.Mutation.EffectID, home, now.Add(10*time.Minute), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE secondbox.runner_commands SET runner_id=$2,payload=$3,updated_at=$4 WHERE id=$1`, commandID, home, payload, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (store *PostgresControlPlaneStore) archiveIdleWorkspaces(ctx context.Context, now time.Time, profiles map[string]int64) error {
	var failures []error
	for profile, idleSeconds := range profiles {
		if idleSeconds < 1 {
			return errors.New("SecondBox archive idle policy must be positive")
		}
		rows, err := store.pool.Query(ctx, `SELECT sandbox.id,sandbox.tenant_ref,sandbox.subject_ref,sandbox.revision,sandbox.generation FROM secondbox.sandboxes sandbox JOIN secondbox.workspaces workspace ON workspace.id=sandbox.workspace_id JOIN secondbox.runners runner ON runner.id=workspace.home_runner_id WHERE sandbox.profile_name=$1 AND sandbox.state='stopped' AND sandbox.desired_state='stopped' AND sandbox.updated_at<$2 AND workspace.state='ready' AND workspace.mutation_state='' AND runner.state IN ('ready','draining') AND runner.active_connection_id<>'' AND NOT EXISTS(SELECT 1 FROM secondbox.snapshots snapshot WHERE snapshot.workspace_id=workspace.id AND snapshot.state<>'deleted') ORDER BY sandbox.updated_at LIMIT 16`, profile, now.Add(-time.Duration(idleSeconds)*time.Second))
		if err != nil {
			return err
		}
		var candidates []ports.WorkspaceArchiveInput
		for rows.Next() {
			var input ports.WorkspaceArchiveInput
			var generation int64
			if err := rows.Scan(&input.SandboxID, &input.Principal.TenantRef, &input.Principal.SubjectRef, &input.ExpectedRevision, &generation); err != nil {
				rows.Close()
				return err
			}
			input.Now = now
			input.ArchiveID, err = newWorkspaceArchiveID()
			if err != nil {
				rows.Close()
				return err
			}
			input.CommandID = input.ArchiveID
			input.FencingToken = make([]byte, 32)
			if _, err := rand.Read(input.FencingToken); err != nil {
				rows.Close()
				return err
			}
			input.IdempotencyKey = input.ArchiveID
			input.RequestHash = input.ArchiveID
			input.IdempotencyEnds = now.Add(24 * time.Hour)
			input.Operation = contracts.Operation{ID: input.ArchiveID, SandboxID: input.SandboxID, Kind: "archive", State: "pending", RequestID: input.ArchiveID, CreatedAt: now, UpdatedAt: now}
			input.AuditEvent = contracts.AuditEvent{ID: input.ArchiveID, TenantRef: input.Principal.TenantRef, SubjectRef: input.Principal.SubjectRef, ActorKind: "system", ActorID: "workspace-archive-policy", Action: "sandbox.archive", ResourceKind: "sandbox", ResourceID: input.SandboxID, Outcome: "accepted", RequestID: input.ArchiveID, Details: map[string]string{}, CreatedAt: now}
			candidates = append(candidates, input)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, candidate := range candidates {
			if _, err := store.ArchiveSandbox(ctx, candidate); err != nil && !errors.Is(err, ports.ErrWorkspaceMutation) && !errors.Is(err, ports.ErrRevisionConflict) && !errors.Is(err, ports.ErrHomeRunnerUnavailable) && !errors.Is(err, ports.ErrSandboxNotStopped) && !errors.Is(err, ports.ErrRelocationSnapshotsPresent) {
				failures = append(failures, fmt.Errorf("SecondBox idle archive %s: %w", candidate.SandboxID, err))
			}
		}
	}
	return errors.Join(failures...)
}

func (store *PostgresControlPlaneStore) collectArchiveObjects(ctx context.Context, now time.Time) error {
	rows, err := store.pool.Query(ctx, `SELECT archive_key FROM secondbox.archive_cleanup WHERE not_before IS NULL OR not_before<=$1 ORDER BY created_at LIMIT 16`, now)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var failures []error
	for _, key := range keys {
		if err := store.archiveTransfers.Delete(ctx, key); err != nil {
			failures = append(failures, fmt.Errorf("SecondBox archive object cleanup %s: %w", key, err))
			if _, retryErr := store.pool.Exec(ctx, `UPDATE secondbox.archive_cleanup SET not_before=$2 WHERE archive_key=$1`, key, now.Add(workspacearchive.ReconciliationRetryDelay)); retryErr != nil {
				failures = append(failures, retryErr)
			}
			continue
		}
		if _, err := store.pool.Exec(ctx, `DELETE FROM secondbox.archive_cleanup WHERE archive_key=$1`, key); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

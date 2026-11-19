package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

func TestLostRestoreTargetReturnsToArchiveWithoutLosingIntent(t *testing.T) {
	storage := openArchiveConfigurationTestStore(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	if err := storage.ConfigureWorkspaceArchives(t.Context(), &archiveTransferTestProvider{}, map[string]int64{"profile-local": 3600}); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []string{"running", "stopped", "deleted", "expired", "recent"} {
		t.Run(intent, func(t *testing.T) {
			workspaceID, sandboxID := seedLocalWorkspace(t, storage, "lost-restore-"+intent, now)
			ids := []string{sandboxID}
			cleanupArchiveLivenessSandboxes(t, storage, &ids)
			operation := localTestOperation("lost-start-"+intent, "start", now)
			operation.SandboxID = sandboxID
			deadline := now.Add(time.Hour)
			if intent == "expired" {
				deadline = now.Add(-time.Second)
			}
			operation.CapacityWaitDeadline = &deadline
			archiveID := "lost-restore-" + intent
			descriptor := &runnerv1.WorkspaceArchiveDescriptor{ArchiveBytes: 4096, ArchiveChecksum: "sha256:" + strings.Repeat("a", 64), ImageChecksum: "sha256:" + strings.Repeat("b", 64)}
			descriptorJSON, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			commandPayload, err := proto.Marshal(&runnerv1.ControlPlaneToRunner{Message: &runnerv1.ControlPlaneToRunner_LocalWorkspace{LocalWorkspace: &runnerv1.LocalWorkspaceCommand{
				CommandVersion: 1, Kind: runnerv1.LocalWorkspaceCommandKind_LOCAL_WORKSPACE_COMMAND_KIND_ARCHIVE_IMPORT,
				OperationId: archiveID, EffectId: archiveID, SandboxId: sandboxID, WorkspaceId: workspaceID,
				ExpectedGeneration: 3, LogicalCapacityBytes: 8 << 30,
				ArchiveTransfer: &runnerv1.WorkspaceArchiveTransfer{ExpiresAtUnixMs: uint64(now.Add(time.Hour).UnixMilli()), ArchiveDescriptor: descriptor},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			tx, err := storage.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err := insertOperation(t.Context(), tx, "tenant-local", "subject-local", operation); err != nil {
				t.Fatal(err)
			}
			seen := now.Add(-time.Hour)
			if intent == "recent" {
				seen = now
			}
			if _, err := tx.Exec(t.Context(), `
 UPDATE secondbox.sandboxes SET desired_state='running' WHERE id=$1;
 UPDATE secondbox.workspaces SET state='restoring',home_runner_id='',archive_key=$2,
 archive_descriptor_json=$3,mutation_kind='archive_restore',mutation_id=$4,mutation_effect_id=$4,
 mutation_operation_id=$5,mutation_state='queued',mutation_expected_generation=3,mutation_target_generation=3 WHERE id=$6;
 UPDATE secondbox.runners SET state='offline',active_connection_id='',last_seen_at=$7 WHERE id='runner-home';
 INSERT INTO secondbox.workspace_archives(id,operation_id,sandbox_id,workspace_id,source_runner_id,target_runner_id,generation,logical_capacity_bytes,archive_key,descriptor_json,state,command_id,fencing_token,created_at,updated_at)
 VALUES($4,$5,$1,$6,'','runner-home',3,8589934592,$2,$3,'restore_queued_sent',$4,'lost-fence',$8,$8);
 INSERT INTO secondbox.runner_commands(id,runner_id,assignment_id,kind,payload,state,target_connection_id,delivery_count,created_at,updated_at)
 VALUES($4,'runner-home',$4,'local-workspace',$9,'delivered','old-connection',1,$8,$8);`, pgx.QueryExecModeSimpleProtocol,
				sandboxID, "workspaces/"+archiveID, string(descriptorJSON), archiveID, operation.ID, workspaceID, seen, now.Add(-time.Hour), commandPayload); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if intent == "stopped" || intent == "deleted" {
				kind := "stop"
				if intent == "deleted" {
					kind = "delete"
				}
				request := localTestOperation("lost-intent-"+intent, kind, now)
				if _, err := storage.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, SandboxID: sandboxID, DesiredState: intent, Operation: request, AuditEvent: testLifecycleAudit(), ExpectedRevision: 1, IdempotencyKey: request.ID, RequestHash: request.ID, IdempotencyEnds: now.Add(time.Hour), Now: now}); err != nil {
					t.Fatal(err)
				}
			}
			if err := storage.ReconcileWorkspaceArchives(t.Context(), now, nil); err != nil {
				t.Fatal(err)
			}
			var residency, mutation, desired, startState, commandState, source, phase string
			if err := storage.pool.QueryRow(t.Context(), `SELECT w.state,w.mutation_kind,s.desired_state,o.state,c.state,a.source_runner_id,a.state
 FROM secondbox.workspaces w JOIN secondbox.sandboxes s ON s.workspace_id=w.id
 JOIN secondbox.operations o ON o.id=$2 JOIN secondbox.runner_commands c ON c.id=$3
 JOIN secondbox.workspace_archives a ON a.id=$3 WHERE w.id=$1`, workspaceID, operation.ID, archiveID).Scan(&residency, &mutation, &desired, &startState, &commandState, &source, &phase); err != nil {
				t.Fatal(err)
			}
			if intent == "recent" {
				if residency != "restoring" {
					t.Fatalf("recent target recovered too early: %s", residency)
				}
				return
			}
			wantResidency := "archived"
			if intent == "deleted" {
				wantResidency = "deleted"
			}
			if residency != wantResidency || commandState != "expired" || source != "runner-home" || phase != "cleanup_queued_sent" {
				t.Fatalf("residency=%s command=%s orphan=%s/%s", residency, commandState, source, phase)
			}
			if intent == "running" {
				if mutation != "start" || desired != "running" || startState != "pending" {
					t.Fatalf("lost original start: mutation=%s desired=%s operation=%s", mutation, desired, startState)
				}
				// A reconnected abandoned target stays quarantined, even when it is
				// the only compatible Runner, until exact orphan deletion succeeds.
				if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET state='ready',active_connection_id='recovered-connection',last_seen_at=$1,
 capabilities_json='["compute","local-workspace","storage","workspace-relocation"]',protocol_versions_json='["8"]',
 capacity_json='{"VCPUCount":4,"MemoryBytes":17179869184,"DiskBytes":68719476736,"Instances":4,"Operations":16}' WHERE id='runner-home'`, now); err != nil {
					t.Fatal(err)
				}
				if err := storage.prepareArchivedWorkspaceStart(t.Context(), sandboxID, now); !errors.Is(err, ports.ErrRelocationTargetUnavailable) {
					t.Fatalf("abandoned target was not quarantined: %v", err)
				}
				seedPlacementRunner(t, storage, "pool-local", "replacement-target", now)
				if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET capabilities_json='["compute","local-workspace","storage","workspace-relocation"]',protocol_versions_json='["8"]',capacity_json='{"VCPUCount":4,"MemoryBytes":17179869184,"DiskBytes":68719476736,"Instances":4,"Operations":16}' WHERE id='replacement-target'`); err != nil {
					t.Fatal(err)
				}
				if err := storage.prepareArchivedWorkspaceStart(t.Context(), sandboxID, now); err != nil {
					t.Fatal(err)
				}
				var target string
				if err := storage.pool.QueryRow(t.Context(), `SELECT target_runner_id FROM secondbox.workspace_archives WHERE workspace_id=$1 AND state='restore_queued'`, workspaceID).Scan(&target); err != nil {
					t.Fatal(err)
				}
				if target != "replacement-target" {
					t.Fatalf("recovery selected %s", target)
				}
			} else if mutation != "" || desired == "running" || startState == "pending" || startState == "running" {
				t.Fatalf("terminal intent remains stuck: mutation=%s desired=%s operation=%s", mutation, desired, startState)
			}
		})
	}
}

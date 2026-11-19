package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	postgresmigrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func openArchiveConfigurationTestStore(t *testing.T) *PostgresControlPlaneStore {
	t.Helper()
	parsed, err := url.Parse(storeTestDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/postgres"
	admin, err := pgx.Connect(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Error(err)
		}
	})
	name := fmt.Sprintf("secondbox_archive_config_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.WithoutCancel(t.Context()), "DROP DATABASE "+identifier+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	parsed.Path = "/" + name
	if err := postgresmigrations.Apply(t.Context(), parsed.String()); err != nil {
		t.Fatal(err)
	}
	storage, err := NewPostgresControlPlaneStore(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	return storage
}

func TestArchiveConfigurationRejectsDrainingHomeWithoutProfileOptIn(t *testing.T) {
	storage := openArchiveConfigurationTestStore(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	if err := storage.ConfigureWorkspaceArchives(t.Context(), &archiveTransferTestProvider{}, nil); err != nil {
		t.Fatal(err)
	}
	_, sandboxID := seedLocalWorkspace(t, storage, "archive-no-profile-opt-in", now)
	ids := []string{sandboxID}
	cleanupArchiveLivenessSandboxes(t, storage, &ids)
	if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET state='draining',drain_phase='draining' WHERE id='runner-home'`); err != nil {
		t.Fatal(err)
	}
	operation := localTestOperation("operation-no-profile-opt-in", "start", now)
	_, err := storage.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{
		Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, SandboxID: sandboxID,
		DesiredState: contracts.SandboxDesiredStateRunning, Operation: operation, AuditEvent: testLifecycleAudit(),
		ExpectedRevision: 1, IdempotencyKey: operation.ID, RequestHash: operation.ID, IdempotencyEnds: now.Add(time.Hour), Now: now,
	})
	if !errors.Is(err, ports.ErrHomeRunnerUnavailable) {
		t.Fatalf("draining home without Profile opt-in admitted: %v", err)
	}
}

func TestArchiveConfigurationProfileOptInIsCopiedBeforeFirstWorkerScan(t *testing.T) {
	for _, optedIn := range []bool{false, true} {
		name := "unconfigured"
		if optedIn {
			name = "configured"
		}
		t.Run(name, func(t *testing.T) {
			storage := openArchiveConfigurationTestStore(t)
			now := time.Now().UTC()
			seedLocalWorkspacePolicyAndRunner(t, storage, now)
			policy := map[string]int64{"unrelated-profile": 3600}
			if optedIn {
				policy["profile-local"] = 3600
			}
			if err := storage.ConfigureWorkspaceArchives(t.Context(), &archiveTransferTestProvider{}, policy); err != nil {
				t.Fatal(err)
			}
			if optedIn {
				delete(policy, "profile-local")
			} else {
				policy["profile-local"] = 3600
			}
			_, sandboxID := seedLocalWorkspace(t, storage, "archive-immutable-policy-"+name, now)
			ids := []string{sandboxID}
			cleanupArchiveLivenessSandboxes(t, storage, &ids)
			if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.runners SET state='draining',drain_phase='draining' WHERE id='runner-home'`); err != nil {
				t.Fatal(err)
			}
			operation := localTestOperation("operation-"+sandboxID, "start", now)
			_, err := storage.SetSandboxDesiredState(t.Context(), ports.LifecycleIntentInput{Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"}, SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateRunning, Operation: operation, AuditEvent: testLifecycleAudit(), ExpectedRevision: 1, IdempotencyKey: operation.ID, RequestHash: operation.ID, IdempotencyEnds: now.Add(time.Hour), Now: now})
			if optedIn && err != nil || !optedIn && !errors.Is(err, ports.ErrHomeRunnerUnavailable) {
				t.Fatalf("Profile opt-in=%t start error=%v", optedIn, err)
			}
			if err := storage.ConfigureWorkspaceArchives(t.Context(), &archiveTransferTestProvider{}, policy); err == nil {
				t.Fatal("archive policy can change after startup")
			}
		})
	}
}

func TestArchiveConfigurationDisableRequiresSettledDurableState(t *testing.T) {
	storage := openArchiveConfigurationTestStore(t)
	now := time.Now().UTC()
	seedLocalWorkspacePolicyAndRunner(t, storage, now)
	workspaceID, sandboxID := seedLocalWorkspace(t, storage, "archive-disable-guard", now)
	ids := []string{sandboxID}
	cleanupArchiveLivenessSandboxes(t, storage, &ids)
	for _, scenario := range []struct {
		name, state, mutation, phase string
		gc, allowed                  bool
	}{
		{name: "resident", state: "ready", allowed: true},
		{name: "archived", state: "archived"},
		{name: "restoring", state: "restoring"},
		{name: "archive-mutation", state: "ready", mutation: "archive"},
		{name: "restore-mutation", state: "ready", mutation: "archive_restore"},
		{name: "queued", state: "ready", phase: "queued"},
		{name: "queued-sent", state: "ready", phase: "queued_sent"},
		{name: "restore-queued", state: "ready", phase: "restore_queued"},
		{name: "restore-sent", state: "ready", phase: "restore_queued_sent"},
		{name: "cleanup", state: "ready", phase: "cleanup_queued"},
		{name: "cleanup-sent", state: "ready", phase: "cleanup_queued_sent"},
		{name: "abort", state: "ready", phase: "abort_queued"},
		{name: "abort-sent", state: "ready", phase: "abort_queued_sent"},
		{name: "object-gc", state: "ready", gc: true},
		{name: "historical-success", state: "ready", phase: "succeeded", allowed: true},
		{name: "historical-failure", state: "ready", phase: "failed", allowed: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if _, err := storage.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET state=$2,mutation_kind=$3 WHERE id=$1`, workspaceID, scenario.state, scenario.mutation); err != nil {
				t.Fatal(err)
			}
			if scenario.phase != "" {
				if _, err := storage.pool.Exec(t.Context(), `INSERT INTO secondbox.workspace_archives(id,operation_id,sandbox_id,workspace_id,source_runner_id,generation,logical_capacity_bytes,archive_key,state,command_id,fencing_token,created_at,updated_at) VALUES('archive-disable-phase','',$1,$2,'',3,8589934592,'config-object',$3,'','f',now(),now())`, sandboxID, workspaceID, scenario.phase); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := storage.pool.Exec(t.Context(), `DELETE FROM secondbox.workspace_archives WHERE id='archive-disable-phase'`); err != nil {
						t.Error(err)
					}
				}()
			}
			if scenario.gc {
				if _, err := storage.pool.Exec(t.Context(), `INSERT INTO secondbox.archive_cleanup(archive_key,created_at) VALUES('config-object',now())`); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := storage.pool.Exec(t.Context(), `DELETE FROM secondbox.archive_cleanup WHERE archive_key='config-object'`); err != nil {
						t.Error(err)
					}
				}()
			}
			fresh, err := NewPostgresControlPlaneStore(t.Context(), storage.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(fresh.Close)
			err = fresh.ConfigureWorkspaceArchives(t.Context(), nil, nil)
			if (err == nil) != scenario.allowed {
				t.Fatalf("disable archive tier allowed=%t error=%v", scenario.allowed, err)
			}
		})
	}
}

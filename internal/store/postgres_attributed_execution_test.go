package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/SecondStack-AI/SecondBox/internal/ports"
	"github.com/SecondStack-AI/SecondBox/pkg/contracts"
	"github.com/jackc/pgx/v5"
)

func TestAttributedStartAdmissionBindsStoppedSandboxAndQualifiedRunner(t *testing.T) {
	baseStore := openStoreTest(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	var existingRunner, existingProfile bool
	if err := baseStore.pool.QueryRow(t.Context(), `SELECT
		EXISTS(SELECT 1 FROM secondbox.runners WHERE id='runner-home'),
		EXISTS(SELECT 1 FROM secondbox.profile_revisions WHERE id='revision-local')`).Scan(&existingRunner, &existingProfile); err != nil {
		t.Fatal(err)
	}
	seedLocalWorkspacePolicyAndRunner(t, baseStore, now)
	t.Cleanup(func() {
		if _, err := baseStore.pool.Exec(context.Background(), `
			DELETE FROM secondbox.runners WHERE id='runner-home' AND NOT $1;
			DELETE FROM secondbox.profile_revisions WHERE id='revision-local' AND NOT $2`, pgx.QueryExecModeSimpleProtocol, existingRunner, existingProfile); err != nil {
			t.Error(err)
		}
	})
	for _, test := range []struct {
		name     string
		state    string
		profile  bool
		runner   bool
		duration time.Duration
		want     error
	}{
		{"accepted", "stopped", true, true, time.Minute, nil},
		{"missing profile permission", "stopped", false, true, time.Minute, ports.ErrInvalidRequest},
		{"unsupported runner", "stopped", true, false, time.Minute, ports.ErrHomeRunnerUnavailable},
		{"failed compute", "failed", true, true, time.Minute, ports.ErrWorkspaceMutation},
		{"active compute", "ready", true, true, time.Minute, ports.ErrWorkspaceMutation},
		{"expired", "stopped", true, true, -time.Second, ports.ErrInvalidRequest},
		{"excess deadline", "stopped", true, true, time.Hour, ports.ErrInvalidRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := openStoreTest(t)
			suffix := "attributed-" + test.name
			_, sandboxID := seedLocalWorkspace(t, store, suffix, now)
			t.Cleanup(func() {
				if _, err := store.pool.Exec(context.Background(), `
					DELETE FROM secondbox.idempotency_records WHERE target_id=$1;
					DELETE FROM secondbox.operation_stage_timings WHERE operation_id=$2;
					DELETE FROM secondbox.operations WHERE id=$2;
					DELETE FROM secondbox.workspaces WHERE sandbox_id=$1;
					DELETE FROM secondbox.sandboxes WHERE id=$1;
					DELETE FROM secondbox.runners WHERE id=$2;
					DELETE FROM secondbox.profile_revisions WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, sandboxID, suffix); err != nil {
					t.Error(err)
				}
			})
			policy := contracts.AttributedExecutionPolicy{Gateway: "gateway", MaximumConnections: 32}
			policyJSON, err := json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.pool.Exec(t.Context(), `
				UPDATE secondbox.sandboxes SET state=$2,egress_context='installation',profile_revision_id=$3 WHERE id=$1`, sandboxID, test.state, suffix); err != nil {
				t.Fatal(err)
			}
			if _, err := store.pool.Exec(t.Context(), `
				INSERT INTO secondbox.profile_revisions(id,profile_name,revision_number,spec_json,created_at)
				SELECT $1,$1,1,spec_json || jsonb_build_object(
				  'execution',jsonb_build_object('maximumDeadlineMilliseconds',120000),
				  'network',jsonb_build_object('requiresTenantEgressContext',true)),created_at
				FROM secondbox.profile_revisions WHERE id='revision-local'`, suffix); err != nil {
				t.Fatal(err)
			}
			if test.profile {
				if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.profile_revisions
					SET spec_json=spec_json || jsonb_build_object('attributedExecution',$1::jsonb)
					WHERE id=$2`, policyJSON, suffix); err != nil {
					t.Fatal(err)
				}
			}
			capabilities := `[]`
			if test.runner {
				capabilities = `["attributed-execution"]`
			}
			if _, err := store.pool.Exec(t.Context(), `INSERT INTO secondbox.runners
				SELECT (jsonb_populate_record(NULL::secondbox.runners,to_jsonb(runner) ||
				jsonb_build_object('id',$1::text,'name',$1::text,'capabilities_json',$2::jsonb))).*
				FROM secondbox.runners runner WHERE id='runner-home'`, suffix, capabilities); err != nil {
				t.Fatal(err)
			}
			if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.workspaces SET home_runner_id=$2 WHERE sandbox_id=$1`, sandboxID, suffix); err != nil {
				t.Fatal(err)
			}
			binding := contracts.AttributedExecutionRequest{AuthorizationRef: "application-command", ExpiresAt: now.Add(test.duration)}
			input := ports.LifecycleIntentInput{
				Principal: contracts.Principal{TenantRef: "tenant-local", SubjectRef: "subject-local"},
				SandboxID: sandboxID, DesiredState: contracts.SandboxDesiredStateRunning,
				Operation: contracts.Operation{ID: suffix, Kind: "start", RequestMetadata: binding.AttributedExecutionMetadata(), CreatedAt: now, UpdatedAt: now},
				Now:       now, ExpectedRevision: 1, IdempotencyKey: "attributed-start", RequestHash: "request-hash", IdempotencyEnds: now.Add(time.Hour),
			}
			operation, err := store.SetSandboxDesiredState(t.Context(), input)
			if !errors.Is(err, test.want) {
				t.Fatalf("admission error = %v; want %v", err, test.want)
			}
			if err != nil {
				var kind string
				if err := store.pool.QueryRow(t.Context(), `SELECT mutation_kind FROM secondbox.workspaces WHERE sandbox_id=$1`, sandboxID).Scan(&kind); err != nil {
					t.Fatal(err)
				}
				if kind != "" {
					t.Fatalf("denied start changed workspace mutation: %q", kind)
				}
				return
			}
			stored, err := store.GetOperation(t.Context(), "tenant-local", "subject-local", operation.ID)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := contracts.ParseAttributedExecutionMetadata(stored.RequestMetadata)
			if err != nil || actual == nil || *actual != binding {
				t.Fatalf("durable binding = %+v, error = %v", actual, err)
			}
		})
	}
}

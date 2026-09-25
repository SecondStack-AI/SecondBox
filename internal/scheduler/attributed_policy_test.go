package scheduler

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	runnerv1 "github.com/SecondStack-AI/SecondBox/gen/runner/v1"
	"github.com/SecondStack-AI/SecondBox/internal/store/rowlock"
	migrations "github.com/SecondStack-AI/SecondBox/migrations/postgres"
	"github.com/jackc/pgx/v5"
)

func TestAttributedAssignmentNumericPolicy(t *testing.T) {
	raw := os.Getenv("SECONDBOX_TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("SECONDBOX_TEST_DATABASE_URL is required")
	}
	admin, err := pgx.Connect(t.Context(), raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	name := fmt.Sprintf("attributed_policy_%d_%d", os.Getpid(), time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+ident); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+ident+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	}()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	if err := migrations.Apply(t.Context(), u.String()); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(t.Context(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	for _, test := range []struct {
		name, head, selection, gateway string
		want                           uint32
		denied                         bool
	}{
		{"old pin adopts default", `{"attributedExecution":{"gateway":"new-gateway","maximumConnections":128},"attributedExecutionCeiling":{"maximumConnections":4096}}`, "", "gateway", 128, false},
		{"selection", `{"attributedExecution":{"gateway":"new-gateway","maximumConnections":128},"attributedExecutionCeiling":{"maximumConnections":4096}}`, `"attributedExecution":{"maximumConnections":256}`, "gateway", 256, false},
		{"tightened ceiling", `{"attributedExecution":{"gateway":"new-gateway","maximumConnections":128},"attributedExecutionCeiling":{"maximumConnections":256}}`, `"attributedExecution":{"maximumConnections":512}`, "gateway", 256, false},
		{"head removed low selection", `{}`, `"attributedExecution":{"maximumConnections":1}`, "gateway", 1, false},
		{"head removed high selection", `{}`, `"attributedExecution":{"maximumConnections":128}`, "gateway", 2, false},
		{"head removed inheritance", `{}`, "", "gateway", 2, false},
		{"gateway cannot change", `{"attributedExecution":{"gateway":"new-gateway","maximumConnections":128}}`, "", "new-gateway", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := db.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			selection := `{"profile":"profile","lifecycle":{"idleSeconds":60,"maximumDurationSeconds":null}`
			if test.selection != "" {
				selection += "," + test.selection
			}
			selection += "}"
			if _, err := tx.Exec(t.Context(), `INSERT INTO secondbox.profile_revisions(id,profile_name,revision_number,spec_json,created_at) VALUES('pin','profile',1,'{"attributedExecution":{"gateway":"gateway","maximumConnections":2}}',now()),('head','profile',2,$1,now()); INSERT INTO secondbox.profiles(name,state,current_revision_id,revision,created_at,updated_at) VALUES('profile','disabled','head',2,now(),now()); INSERT INTO secondbox.subjects(tenant_ref,ref,state,cleanup_state,cleanup_operation_id,quota_json,metadata_json,sandbox_policy_json,revision,created_at,updated_at) VALUES('tenant','subject','active','none','','{}','{}',$2,1,now(),now())`, pgx.QueryExecModeSimpleProtocol, test.head, selection); err != nil {
				t.Fatal(err)
			}
			command := &runnerv1.AttributedExecution{Gateway: test.gateway, MaximumConnections: 2}
			locked := rowlock.SandboxWorkspace{ProfileRevisionID: "pin", TenantRef: "tenant", SubjectRef: "subject"}
			err = resolveAttributedConnections(t.Context(), tx, locked, command)
			if test.denied {
				if err == nil {
					t.Fatal("accepted unpinned gateway")
				}
				return
			}
			if err != nil || command.MaximumConnections != test.want || command.Gateway != "gateway" {
				t.Fatalf("resolved %v, %v", command, err)
			}
			if _, err := tx.Exec(t.Context(), `UPDATE secondbox.subjects SET sandbox_policy_json=jsonb_set(sandbox_policy_json,'{profile}','"other-profile"')`); err != nil {
				t.Fatal(err)
			}
			if err := resolveAttributedConnections(t.Context(), tx, locked, command); err != nil {
				t.Fatal(err)
			}
			if command.MaximumConnections != 128 && command.MaximumConnections != 2 {
				t.Fatalf("other Profile leaked selection: %v", command)
			}
			if _, err := tx.Exec(t.Context(), `UPDATE secondbox.profile_revisions SET spec_json='{}' WHERE id='pin'`); err != nil {
				t.Fatal(err)
			}
			if err := resolveAttributedConnections(t.Context(), tx, locked, command); err == nil {
				t.Fatal("head granted attribution to non-attributed pin")
			}
		})
	}
}

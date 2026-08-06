package postgresmigrations

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The retag migration must move exactly the commands the current stop effects
// reference: the assignment reconciler's own fence commands and superseded
// stop retries (already terminal) keep kind='fence'.
func TestLifecycleFenceCommandKindRetagsOnlyReferencedStopCommands(t *testing.T) {
	connection := newGuardDatabase(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	if _, err := connection.Exec(ctx, `
		INSERT INTO secondbox.lifecycle_effects (
			id,sandbox_id,generation,kind,state,assignment_id,instance_id,runner_id,
			command_id,storage_object_id,fencing_token,retry_count,retry_limit,effect_deadline,
			claim_owner,claim_expires_at,failure_class,failure_message,payload_json,evidence_json,
			created_at,updated_at
		) VALUES (
			'effect-stop','sandbox-one',3,'stop','queued','assignment-one','instance-one',
			'runner-one','command-stop-current','',$2,1,2,$3,'',$1,'','','{}','{}',$1,$1
		);
		INSERT INTO secondbox.runner_commands (
			id,runner_id,assignment_id,kind,payload,state,target_connection_id,
			delivery_count,created_at,updated_at,delivered_at
		) VALUES
			('command-stop-current','runner-one','assignment-one','fence',$4,'pending','',0,$1,$1,NULL),
			('command-stop-superseded','runner-one','assignment-one','fence',$4,'expired','',1,$1,$1,NULL),
			('command-reconciler-fence','runner-one','assignment-two','fence',$4,'pending','',0,$1,$1,NULL)`,
		pgx.QueryExecModeSimpleProtocol,
		now, []byte("01234567890123456789012345678901"), now.Add(time.Minute), []byte{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(
		ctx, migrationSQL(t, "0010_lifecycle_fence_command_kind.sql"),
	); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	rows, err := connection.Query(
		ctx, "SELECT id,kind FROM secondbox.runner_commands ORDER BY id",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind string
		if err := rows.Scan(&id, &kind); err != nil {
			t.Fatal(err)
		}
		kinds[id] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if kinds["command-stop-current"] != "lifecycle_fence" ||
		kinds["command-stop-superseded"] != "fence" ||
		kinds["command-reconciler-fence"] != "fence" {
		t.Fatalf("retagged command kinds = %#v", kinds)
	}
}

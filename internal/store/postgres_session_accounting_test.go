package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestSweepSessionAccountingBoundsEachPassAndRetainsFreshRows(t *testing.T) {
	controlPlaneStore := openStoreTest(t)
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		DELETE FROM secondbox.activity_touches;
		DELETE FROM secondbox.idempotency_records`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	for index := range 3 {
		if _, err := controlPlaneStore.pool.Exec(t.Context(), `
			INSERT INTO secondbox.idempotency_records (
				tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
				response_resource_id,created_at,expires_at
			) VALUES ('sweep-tenant','sweep-subject','sweep.test','', $1,'hash','resource',$2,$3)`,
			fmt.Sprintf("expired-%d", index), now.Add(-48*time.Hour), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 3 {
		if _, err := controlPlaneStore.pool.Exec(t.Context(), `
			INSERT INTO secondbox.activity_touches (
				tenant_ref,subject_ref,sandbox_id,generation,lease_id,idempotency_key,
				request_hash,last_activity_at,created_at
			) VALUES ('sweep-tenant','sweep-subject','sweep-sandbox',1,'sweep-lease',$1,
			          'hash',$2,$2)`, fmt.Sprintf("aged-%d", index), now.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := controlPlaneStore.pool.Exec(t.Context(), `
		INSERT INTO secondbox.idempotency_records (
			tenant_ref,subject_ref,operation,target_id,idempotency_key,request_hash,
			response_resource_id,created_at,expires_at
		) VALUES ('sweep-tenant','sweep-subject','sweep.test','','fresh','hash','resource',$1,$2);
		INSERT INTO secondbox.activity_touches (
			tenant_ref,subject_ref,sandbox_id,generation,lease_id,idempotency_key,
			request_hash,last_activity_at,created_at
		) VALUES ('sweep-tenant','sweep-subject','sweep-sandbox',1,'sweep-lease','fresh',
		          'hash',$1,$1)`, pgx.QueryExecModeSimpleProtocol, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	for pass := range 3 {
		deleted, err := controlPlaneStore.SweepSessionAccounting(t.Context(), now, 24*time.Hour, 2)
		if err != nil {
			t.Fatal(err)
		}
		if deleted != 2 {
			t.Fatalf("pass %d deleted = %d, want 2", pass, deleted)
		}
	}
	deleted, err := controlPlaneStore.SweepSessionAccounting(t.Context(), now, 24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("final pass deleted = %d, want 0", deleted)
	}
	var idempotencyCount, activityCount int
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.idempotency_records
		WHERE tenant_ref='sweep-tenant'`).Scan(&idempotencyCount); err != nil {
		t.Fatal(err)
	}
	if err := controlPlaneStore.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM secondbox.activity_touches
		WHERE tenant_ref='sweep-tenant'`).Scan(&activityCount); err != nil {
		t.Fatal(err)
	}
	if idempotencyCount != 1 || activityCount != 1 {
		t.Fatalf("fresh rows = idempotency:%d activity:%d, want 1 each", idempotencyCount, activityCount)
	}
}

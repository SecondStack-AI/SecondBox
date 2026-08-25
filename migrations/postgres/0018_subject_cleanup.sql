ALTER TABLE secondbox.subjects
    ADD COLUMN cleanup_operation_id text NOT NULL DEFAULT '';
ALTER TABLE secondbox.subjects
    ALTER COLUMN cleanup_operation_id DROP DEFAULT;

CREATE TABLE secondbox.subject_cleanup_operations (
    operation_id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    stage text NOT NULL,
    reconcile_owner text NOT NULL,
    reconcile_claim_expires_at timestamptz NOT NULL,
    next_reconcile_at timestamptz NOT NULL,
    retry_count bigint NOT NULL,
    retry_limit bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX subject_cleanup_operations_subject_idx
    ON secondbox.subject_cleanup_operations (tenant_ref, subject_ref);
CREATE INDEX subject_cleanup_operations_due_idx
    ON secondbox.subject_cleanup_operations (next_reconcile_at, operation_id)
    WHERE stage NOT IN ('succeeded','failed');

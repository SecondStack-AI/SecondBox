CREATE TABLE secondbox.workspace_relocations (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    workspace_id text NOT NULL,
    operation_id text NOT NULL,
    source_runner_id text NOT NULL,
    target_runner_id text NOT NULL,
    generation bigint NOT NULL,
    logical_capacity_bytes bigint NOT NULL,
    state text NOT NULL,
    export_command_id text NOT NULL,
    cleanup_command_id text NOT NULL,
    fencing_token bytea NOT NULL,
    checksum text NOT NULL,
    failure_code text NOT NULL,
    failure_message text NOT NULL,
    retry_count bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz
);
CREATE UNIQUE INDEX workspace_relocations_operation_idx
    ON secondbox.workspace_relocations (operation_id);
CREATE INDEX workspace_relocations_source_state_idx
    ON secondbox.workspace_relocations (source_runner_id, state, updated_at, id);
CREATE INDEX workspace_relocations_target_state_idx
    ON secondbox.workspace_relocations (target_runner_id, state, updated_at, id);

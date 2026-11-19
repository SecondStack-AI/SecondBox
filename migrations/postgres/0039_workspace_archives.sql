ALTER TABLE secondbox.workspaces
    ADD COLUMN archive_key text NOT NULL DEFAULT '',
    ADD COLUMN archive_descriptor_json jsonb,
    ADD COLUMN archived_at timestamptz;

CREATE TABLE secondbox.workspace_archives (
    id text PRIMARY KEY,
    operation_id text NOT NULL,
    sandbox_id text NOT NULL,
    workspace_id text NOT NULL,
    source_runner_id text NOT NULL,
    target_runner_id text NOT NULL DEFAULT '',
    generation bigint NOT NULL,
    logical_capacity_bytes bigint NOT NULL,
    archive_key text NOT NULL,
    descriptor_json jsonb,
    state text NOT NULL,
    command_id text NOT NULL,
    fencing_token bytea NOT NULL,
    error_code text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX workspace_archives_operation_idx ON secondbox.workspace_archives(operation_id);
CREATE INDEX workspace_archives_pending_idx ON secondbox.workspace_archives(state,updated_at,id)
    WHERE state NOT IN ('succeeded','failed');

CREATE TABLE secondbox.archive_cleanup (
    archive_key text PRIMARY KEY,
    created_at timestamptz NOT NULL
);

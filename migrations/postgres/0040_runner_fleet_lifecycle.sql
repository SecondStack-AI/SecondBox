ALTER TABLE secondbox.runners ADD COLUMN fleet_state text NOT NULL DEFAULT 'active';
ALTER TABLE secondbox.runners ADD COLUMN retired_at timestamptz;

CREATE TABLE secondbox.fleet_node_terminations (
    instance_id text PRIMARY KEY,
    pool_name text NOT NULL,
    runner_id text NOT NULL,
    group_name text NOT NULL,
    fleet_ref text NOT NULL,
    volume_ids jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    completed_at timestamptz
);

CREATE TABLE secondbox.fleet_enrollments (
    token_sha256 bytea PRIMARY KEY,
    pool_name text NOT NULL,
    runner_id text NOT NULL,
    instance_id text NOT NULL,
    csr_sha256 bytea,
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    response_json jsonb
);

CREATE UNIQUE INDEX fleet_enrollment_runner ON secondbox.fleet_enrollments(runner_id);

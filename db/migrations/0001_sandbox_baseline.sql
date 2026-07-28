CREATE TABLE sandbox.schema_migrations (
    version text NOT NULL,
    checksum_sha256 text NOT NULL,
    applied_at timestamptz NOT NULL
);

CREATE UNIQUE INDEX schema_migrations_version_idx
    ON sandbox.schema_migrations(version);

CREATE TABLE sandbox.resource_classes (
    id text PRIMARY KEY,
    cpu_millis bigint NOT NULL,
    memory_bytes bigint NOT NULL,
    disk_bytes bigint NOT NULL,
    process_limit bigint NOT NULL,
    max_exposed_ports bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE sandbox.lifecycle_policies (
    id text PRIMARY KEY,
    idle_stop_after_seconds bigint NOT NULL,
    retention_seconds bigint NOT NULL,
    stop_compute_when_idle boolean NOT NULL,
    retain_on_explicit_stop boolean NOT NULL,
    keep_running_without_wake boolean NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE sandbox.workspaces (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    storage_ref text NOT NULL,
    generation bigint NOT NULL,
    retain_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE sandbox.environments (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    environment_key text NOT NULL,
    workspace_id text NOT NULL,
    image_ref text NOT NULL,
    toolchain_ref text NOT NULL,
    resource_class_id text NOT NULL,
    lifecycle_policy_id text NOT NULL,
    desired_state text NOT NULL,
    state text NOT NULL,
    current_generation bigint NOT NULL,
    current_instance_id text NOT NULL,
    snapshot_id text NOT NULL,
    exposed_ports_json jsonb NOT NULL,
    metadata_json jsonb NOT NULL,
    last_activity_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    UNIQUE (tenant_ref, subject_ref, environment_key)
);

CREATE TABLE sandbox.instances (
    id text PRIMARY KEY,
    environment_id text NOT NULL,
    generation bigint NOT NULL,
    state text NOT NULL,
    backend_ref text NOT NULL,
    failure_code text NOT NULL,
    prepared_at timestamptz NOT NULL,
    ready_at timestamptz,
    stopped_at timestamptz,
    updated_at timestamptz NOT NULL,
    UNIQUE (environment_id, generation)
);

CREATE TABLE sandbox.leases (
    id text PRIMARY KEY,
    environment_id text NOT NULL,
    generation bigint NOT NULL,
    holder_ref text NOT NULL,
    state text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE sandbox.snapshots (
    id text PRIMARY KEY,
    environment_id text NOT NULL,
    workspace_id text NOT NULL,
    generation bigint NOT NULL,
    parent_snapshot_id text NOT NULL,
    opaque_ref text NOT NULL,
    content_hash text NOT NULL,
    size_bytes bigint NOT NULL,
    metadata_json jsonb NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE sandbox.workspace_versions (
    environment_id text NOT NULL,
    logical_version bigint NOT NULL,
    source_generation bigint NOT NULL,
    terminal_turn_id text NOT NULL,
    terminal_status text NOT NULL,
    workspace_present boolean NOT NULL,
    dirty boolean NOT NULL,
    content_hash text NOT NULL,
    snapshot_id text NOT NULL,
    snapshot_logical_version bigint NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (environment_id, logical_version),
    UNIQUE (environment_id, terminal_turn_id)
);

CREATE TABLE sandbox.artifacts (
    id text PRIMARY KEY,
    environment_id text NOT NULL,
    generation bigint NOT NULL,
    name text NOT NULL,
    mime_type text NOT NULL,
    size_bytes bigint NOT NULL,
    sha256 text NOT NULL,
    opaque_ref text NOT NULL,
    metadata_json jsonb NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE INDEX environments_state_activity_idx
    ON sandbox.environments (state, last_activity_at, id);
CREATE INDEX environments_policy_state_idx
    ON sandbox.environments (lifecycle_policy_id, state, updated_at, id);
CREATE INDEX instances_environment_generation_idx
    ON sandbox.instances (environment_id, generation DESC);
CREATE INDEX leases_environment_state_expiry_idx
    ON sandbox.leases (environment_id, state, expires_at);
CREATE INDEX snapshots_environment_generation_idx
    ON sandbox.snapshots (environment_id, generation DESC, created_at DESC);
CREATE INDEX workspace_versions_environment_current_idx
    ON sandbox.workspace_versions (environment_id, logical_version DESC);
CREATE INDEX artifacts_environment_generation_idx
    ON sandbox.artifacts (environment_id, generation, created_at DESC);
INSERT INTO sandbox.resource_classes (
    id, cpu_millis, memory_bytes, disk_bytes, process_limit, max_exposed_ports, created_at, updated_at
) VALUES
    ('agent-standard', 2000, 4294967296, 8589934592, 256, 0, '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z'),
    ('chat-standard', 1000, 536870912, 4294967296, 128, 0, '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z'),
    ('coding-standard', 4000, 8589934592, 34359738368, 512, 16, '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z')
ON CONFLICT (id) DO NOTHING;

INSERT INTO sandbox.lifecycle_policies (
    id, idle_stop_after_seconds, retention_seconds, stop_compute_when_idle,
    retain_on_explicit_stop, keep_running_without_wake, created_at, updated_at
) VALUES
    ('agent-compartment', 300, 604800, true, true, false, '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z'),
    ('chat-thread', 120, 86400, true, true, false, '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z'),
    ('coding-environment', 0, 7776000, false, true, true, '2026-07-26T00:00:00Z', '2026-07-26T00:00:00Z')
ON CONFLICT (id) DO NOTHING;

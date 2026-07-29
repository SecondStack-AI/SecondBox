CREATE SCHEMA IF NOT EXISTS secondbox;

CREATE TABLE secondbox.schema_migrations (
    version text NOT NULL,
    checksum_sha256 text NOT NULL,
    applied_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX schema_migrations_version_idx ON secondbox.schema_migrations (version);

CREATE TABLE secondbox.subject_quotas (
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    max_sandboxes bigint NOT NULL,
    max_active_instances bigint NOT NULL,
    max_cpu_millis bigint NOT NULL,
    max_memory_bytes bigint NOT NULL,
    max_artifact_bytes bigint NOT NULL,
    max_snapshots bigint NOT NULL,
    max_artifacts bigint NOT NULL,
    max_port_sessions bigint NOT NULL,
    max_concurrent_operations bigint NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_ref, subject_ref)
);

CREATE TABLE secondbox.profiles (
    name text PRIMARY KEY,
    state text NOT NULL,
    current_revision_id text NOT NULL,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE TABLE secondbox.profile_revisions (
    id text PRIMARY KEY,
    profile_name text NOT NULL,
    revision_number bigint NOT NULL,
    spec_json jsonb NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX profile_revisions_profile_number_idx ON secondbox.profile_revisions (profile_name, revision_number);
CREATE INDEX profiles_created_idx ON secondbox.profiles (created_at, name);

CREATE TABLE secondbox.runner_pools (
    name text PRIMARY KEY,
    state text NOT NULL,
    architectures_json jsonb NOT NULL,
    capabilities_json jsonb NOT NULL,
    capacity_policy_json jsonb NOT NULL,
    ready_runner_count bigint NOT NULL,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX runner_pools_created_idx ON secondbox.runner_pools (created_at, name);

CREATE TABLE secondbox.runners (
    id text PRIMARY KEY,
    pool_name text NOT NULL,
    name text NOT NULL,
    state text NOT NULL,
    architectures_json jsonb NOT NULL,
    capabilities_json jsonb NOT NULL,
    capacity_json jsonb NOT NULL,
    protocol_versions_json jsonb NOT NULL,
    guest_protocol_minimum bigint NOT NULL,
    guest_protocol_maximum bigint NOT NULL,
    software_version text NOT NULL,
    active_connection_id text NOT NULL,
    last_sequence bigint NOT NULL,
    drain_phase text NOT NULL,
    reserved_capacity_json jsonb NOT NULL,
    artifact_cache_json jsonb NOT NULL,
    sandbox_start_sample_count bigint NOT NULL,
    sandbox_start_p95_milliseconds bigint NOT NULL,
    last_seen_at timestamptz,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX runners_pool_state_idx ON secondbox.runners (pool_name, state, id);
CREATE INDEX runners_pool_created_idx ON secondbox.runners (pool_name, created_at, id);
CREATE INDEX runners_created_idx ON secondbox.runners (created_at, id);

CREATE TABLE secondbox.runner_connections (
    id text PRIMARY KEY,
    runner_id text NOT NULL,
    credential_serial text NOT NULL,
    protocol_version bigint NOT NULL,
    state text NOT NULL,
    last_sequence bigint NOT NULL,
    last_control_sequence bigint NOT NULL,
    connected_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    disconnected_at timestamptz
);
CREATE INDEX runner_connections_runner_state_idx ON secondbox.runner_connections (runner_id, state, id);

CREATE TABLE secondbox.runner_messages (
    connection_id text NOT NULL,
    message_id text NOT NULL,
    sequence bigint NOT NULL,
    kind text NOT NULL,
    observed_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX runner_messages_connection_message_idx ON secondbox.runner_messages (connection_id, message_id);
CREATE UNIQUE INDEX runner_messages_connection_sequence_idx ON secondbox.runner_messages (connection_id, sequence);

CREATE TABLE secondbox.runner_commands (
    id text PRIMARY KEY,
    runner_id text NOT NULL,
    assignment_id text NOT NULL,
    kind text NOT NULL,
    payload bytea NOT NULL,
    state text NOT NULL,
    target_connection_id text NOT NULL,
    delivery_count bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    delivered_at timestamptz
);
CREATE INDEX runner_commands_delivery_idx ON secondbox.runner_commands (runner_id, state, created_at, id);

CREATE TABLE secondbox.workspaces (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    home_runner_id text NOT NULL,
    state text NOT NULL,
    logical_capacity_bytes bigint NOT NULL,
    generation bigint NOT NULL,
    mutation_kind text NOT NULL,
    mutation_id text NOT NULL,
    mutation_effect_id text NOT NULL,
    mutation_operation_id text NOT NULL,
    mutation_expected_generation bigint,
    mutation_target_generation bigint,
    mutation_state text NOT NULL,
    local_receipt_json jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX workspaces_sandbox_idx ON secondbox.workspaces (sandbox_id);
CREATE INDEX workspaces_subject_idx ON secondbox.workspaces (tenant_ref, subject_ref, id);
CREATE INDEX workspaces_home_state_idx ON secondbox.workspaces (home_runner_id, state, id);
CREATE INDEX workspaces_pending_mutation_idx
    ON secondbox.workspaces (home_runner_id, mutation_state, updated_at, id)
    WHERE mutation_state <> '';

CREATE TABLE secondbox.sandboxes (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    profile_name text NOT NULL,
    profile_revision_id text NOT NULL,
    state text NOT NULL,
    desired_state text NOT NULL,
    generation bigint NOT NULL,
    workspace_id text NOT NULL,
    current_instance_id text NOT NULL,
    metadata_json jsonb NOT NULL,
    compatibility_summary_json jsonb NOT NULL,
    last_activity_at timestamptz,
    lifecycle_termination_reason text,
    lifecycle_failure_class text,
    lifecycle_failure_message text,
    lifecycle_intent_kind text,
    lifecycle_action text,
    lifecycle_request_metadata_json jsonb,
    drain_started_at timestamptz,
    reconcile_owner text,
    reconcile_claim_expires_at timestamptz,
    next_reconcile_at timestamptz,
    reconcile_retry_count bigint,
    reconcile_retry_limit bigint,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz
);
CREATE INDEX sandboxes_subject_created_idx ON secondbox.sandboxes (tenant_ref, subject_ref, created_at, id);
CREATE INDEX sandboxes_subject_state_idx ON secondbox.sandboxes (tenant_ref, subject_ref, state, id);
CREATE INDEX sandboxes_profile_state_idx ON secondbox.sandboxes (profile_name, state, id);

CREATE TABLE secondbox.lifecycle_effects (
    id text PRIMARY KEY,
    sandbox_id text NOT NULL,
    generation bigint NOT NULL,
    kind text NOT NULL,
    state text NOT NULL,
    assignment_id text NOT NULL,
    instance_id text NOT NULL,
    runner_id text NOT NULL,
    command_id text NOT NULL,
    storage_object_id text NOT NULL,
    fencing_token bytea NOT NULL,
    retry_count bigint NOT NULL,
    retry_limit bigint NOT NULL,
    effect_deadline timestamptz NOT NULL,
    claim_owner text NOT NULL,
    claim_expires_at timestamptz NOT NULL,
    failure_class text NOT NULL,
    failure_message text NOT NULL,
    payload_json jsonb NOT NULL,
    evidence_json jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX lifecycle_effects_due_idx
    ON secondbox.lifecycle_effects (state, claim_expires_at, created_at, id);
CREATE INDEX lifecycle_effects_sandbox_generation_idx
    ON secondbox.lifecycle_effects (sandbox_id, generation, kind, created_at, id);

CREATE TABLE secondbox.instances (
    id text PRIMARY KEY,
    sandbox_id text NOT NULL,
    generation bigint NOT NULL,
    state text NOT NULL,
    guest_liveness text,
    termination_reason text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    ready_at timestamptz,
    guest_heartbeat_at timestamptz,
    maximum_duration_at timestamptz,
    stopped_at timestamptz
);
CREATE UNIQUE INDEX instances_sandbox_generation_idx ON secondbox.instances (sandbox_id, generation);

CREATE TABLE secondbox.instance_terminal_events (
    instance_id text PRIMARY KEY,
    sandbox_id text NOT NULL,
    generation bigint NOT NULL,
    assignment_id text NOT NULL,
    runner_id text NOT NULL,
    reason text NOT NULL,
    evidence_digest text NOT NULL,
    observed_at timestamptz NOT NULL,
    request_id text NOT NULL,
    operation_id text NOT NULL,
    lease_id text NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX instance_terminal_events_assignment_idx ON secondbox.instance_terminal_events (assignment_id);

CREATE TABLE secondbox.assignments (
    id text PRIMARY KEY,
    sandbox_id text NOT NULL,
    instance_id text NOT NULL,
    runner_id text NOT NULL,
    profile_revision_id text NOT NULL,
    backend_kind text NOT NULL,
    backend_reference text NOT NULL,
    generation bigint NOT NULL,
    fencing_token bytea NOT NULL,
    state text NOT NULL,
    capability_snapshot_json jsonb NOT NULL,
    resolved_artifacts_json jsonb NOT NULL,
    release_proof_json jsonb NOT NULL,
    failure_class text NOT NULL,
    retry_count bigint NOT NULL,
    retry_limit bigint NOT NULL,
    operation_deadline timestamptz NOT NULL,
    claim_expires_at timestamptz NOT NULL,
    reconcile_owner text NOT NULL,
    reconcile_claim_expires_at timestamptz NOT NULL,
    next_reconcile_at timestamptz NOT NULL,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX assignments_sandbox_generation_idx ON secondbox.assignments (sandbox_id, generation);
CREATE INDEX assignments_runner_state_idx ON secondbox.assignments (runner_id, state, id);
CREATE INDEX assignments_reconcile_idx ON secondbox.assignments (next_reconcile_at, state, id);

CREATE TABLE secondbox.assignment_stage_timings (
    assignment_id text NOT NULL,
    operation_id text NOT NULL,
    sandbox_id text NOT NULL,
    stage text NOT NULL,
    observed_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    PRIMARY KEY (assignment_id, stage)
);
CREATE INDEX assignment_stage_timings_operation_idx
    ON secondbox.assignment_stage_timings (operation_id, observed_at, assignment_id, stage);
CREATE INDEX assignment_stage_timings_sandbox_idx
    ON secondbox.assignment_stage_timings (sandbox_id, observed_at, assignment_id, stage);

CREATE TABLE secondbox.leases (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    generation bigint NOT NULL,
    state text NOT NULL,
    expires_at timestamptz NOT NULL,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX leases_sandbox_state_expiry_idx ON secondbox.leases (sandbox_id, state, expires_at, id);

CREATE TABLE secondbox.activity_sessions (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    generation bigint NOT NULL,
    kind text NOT NULL,
    state text NOT NULL,
    lease_id text NOT NULL,
    last_activity_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    closed_at timestamptz
);
CREATE INDEX activity_sessions_sandbox_state_idx
    ON secondbox.activity_sessions (sandbox_id, generation, state, last_activity_at, id);

CREATE TABLE secondbox.activity_touches (
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    generation bigint NOT NULL,
    lease_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    last_activity_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX activity_touches_idempotency_idx
    ON secondbox.activity_touches (tenant_ref, subject_ref, sandbox_id, idempotency_key);

CREATE TABLE secondbox.snapshots (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    workspace_id text NOT NULL,
    home_runner_id text NOT NULL,
    operation_id text NOT NULL,
    effect_id text NOT NULL,
    runner_receipt_json jsonb NOT NULL,
    source_generation bigint NOT NULL,
    name text NOT NULL,
    size_bytes bigint NOT NULL,
    metadata_json jsonb NOT NULL,
    state text NOT NULL,
    retain_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    retention_ended_at timestamptz
);
CREATE INDEX snapshots_project_sandbox_created_idx ON secondbox.snapshots (tenant_ref, subject_ref, sandbox_id, created_at DESC, id DESC);
CREATE UNIQUE INDEX snapshots_operation_idx ON secondbox.snapshots (operation_id);
CREATE INDEX snapshots_home_state_idx ON secondbox.snapshots (home_runner_id, state, retain_until, id);

CREATE TABLE secondbox.workspace_restores (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    workspace_id text NOT NULL,
    snapshot_id text NOT NULL,
    home_runner_id text NOT NULL,
    operation_id text NOT NULL,
    prepare_effect_id text NOT NULL,
    swap_effect_id text NOT NULL,
    finalize_effect_id text NOT NULL,
    abort_effect_id text NOT NULL,
    prepare_command_id text NOT NULL,
    swap_command_id text NOT NULL,
    finalize_command_id text NOT NULL,
    abort_command_id text NOT NULL,
    expected_generation bigint NOT NULL,
    target_generation bigint NOT NULL,
    state text NOT NULL,
    prepare_receipt_json jsonb NOT NULL,
    swap_receipt_json jsonb NOT NULL,
    finalize_receipt_json jsonb NOT NULL,
    abort_receipt_json jsonb NOT NULL,
    failure_class text NOT NULL,
    failure_message text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    database_committed_at timestamptz,
    finalized_at timestamptz,
    failed_at timestamptz
);
CREATE UNIQUE INDEX workspace_restores_operation_idx ON secondbox.workspace_restores (operation_id);
CREATE INDEX workspace_restores_home_state_idx
    ON secondbox.workspace_restores (home_runner_id, state, updated_at, id);

CREATE TABLE secondbox.artifacts (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    source_generation bigint NOT NULL,
    name text NOT NULL,
    media_type text NOT NULL,
    size_bytes bigint NOT NULL,
    sha256 text NOT NULL,
    storage_key text NOT NULL,
    state text,
    metadata_json jsonb NOT NULL,
    retain_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    published_at timestamptz,
    garbage_collection_marked_at timestamptz,
    garbage_collected_at timestamptz
);
CREATE INDEX artifacts_project_sandbox_created_idx ON secondbox.artifacts (tenant_ref, subject_ref, sandbox_id, created_at DESC, id DESC);
CREATE INDEX artifacts_gc_idx ON secondbox.artifacts (state, retain_until, id);

CREATE TABLE secondbox.port_sessions (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    profile_revision_id text NOT NULL,
    data_plane_session_id text NOT NULL,
    lease_id text NOT NULL,
    generation bigint NOT NULL,
    name text NOT NULL,
    guest_port bigint NOT NULL,
    protocol text NOT NULL,
    stream_window_bytes bigint NOT NULL,
    client_credit_bytes bigint NOT NULL,
    client_bytes bigint NOT NULL,
    runner_bytes bigint NOT NULL,
    state text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    connected_at timestamptz,
    closed_at timestamptz
);
CREATE UNIQUE INDEX port_sessions_idempotency_idx
    ON secondbox.port_sessions (tenant_ref, subject_ref, sandbox_id, idempotency_key);
CREATE INDEX port_sessions_project_state_idx ON secondbox.port_sessions (tenant_ref, subject_ref, state, expires_at, id);
CREATE INDEX port_sessions_sandbox_state_idx ON secondbox.port_sessions (sandbox_id, state, expires_at, id);

CREATE TABLE secondbox.data_plane_sessions (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    profile_revision_id text NOT NULL,
    assignment_id text NOT NULL,
    instance_id text NOT NULL,
    runner_id text NOT NULL,
    generation bigint NOT NULL,
    fencing_token bytea NOT NULL,
    request_id text NOT NULL,
    lease_id text NOT NULL,
    kind text NOT NULL,
    operation text NOT NULL,
    stream_id text NOT NULL,
    state text NOT NULL,
    priority bigint NOT NULL,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    deadline_at timestamptz NOT NULL,
    maximum_response_bytes bigint NOT NULL,
    maximum_request_bytes bigint NOT NULL,
    stream_window_bytes bigint NOT NULL,
    response_credit_bytes bigint NOT NULL,
    request_stream_bytes bigint NOT NULL,
    request_stream_closed boolean NOT NULL,
    detachable boolean NOT NULL,
    terminal_detach_seconds bigint NOT NULL,
    attachment_id text NOT NULL,
    attached_at timestamptz,
    detached_at timestamptz,
    detach_expires_at timestamptz,
    outbound_bytes bigint NOT NULL,
    inbound_bytes bigint NOT NULL,
    next_inbound_sequence bigint NOT NULL,
    terminal_kind text NOT NULL,
    terminal_detail text NOT NULL,
    exit_code integer NOT NULL,
    signal integer NOT NULL,
    spawn_failure_reason text NOT NULL,
    elapsed_milliseconds bigint NOT NULL,
    limit_bytes bigint NOT NULL,
    infrastructure_failure_reason text NOT NULL,
    retryable boolean NOT NULL,
    terminal_message text NOT NULL,
    stdout_bytes bytea NOT NULL,
    stderr_bytes bytea NOT NULL,
    content_bytes bytea NOT NULL,
    metadata_json jsonb NOT NULL,
    request_json jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    retain_until timestamptz NOT NULL
);
CREATE UNIQUE INDEX data_plane_sessions_stream_idx
    ON secondbox.data_plane_sessions (kind, assignment_id, id, stream_id);
CREATE UNIQUE INDEX data_plane_sessions_idempotency_idx
    ON secondbox.data_plane_sessions (tenant_ref, subject_ref, operation, sandbox_id, idempotency_key)
    WHERE idempotency_key<>'';
CREATE INDEX data_plane_sessions_runner_state_idx
    ON secondbox.data_plane_sessions (runner_id, state, priority, created_at, id);
CREATE INDEX data_plane_sessions_retention_idx
    ON secondbox.data_plane_sessions (retain_until, state, id);

CREATE TABLE secondbox.data_plane_frames (
    id text PRIMARY KEY,
    session_id text NOT NULL,
    direction text NOT NULL,
    sequence bigint NOT NULL,
    payload_hash text NOT NULL,
    payload bytea NOT NULL,
    payload_bytes bigint NOT NULL,
    priority bigint NOT NULL,
    state text NOT NULL,
    claim_owner text NOT NULL,
    claim_expires_at timestamptz,
    delivery_count bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    delivered_at timestamptz,
    consumed_at timestamptz
);
CREATE UNIQUE INDEX data_plane_frames_sequence_idx
    ON secondbox.data_plane_frames (session_id, direction, sequence);
CREATE INDEX data_plane_frames_delivery_idx
    ON secondbox.data_plane_frames (direction, state, priority, created_at, id);

CREATE TABLE secondbox.operations (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    sandbox_id text NOT NULL,
    snapshot_id text NOT NULL,
    kind text NOT NULL,
    state text NOT NULL,
    request_id text NOT NULL,
    request_metadata_json jsonb,
    error_code text NOT NULL,
    error_message text NOT NULL,
    retryable boolean NOT NULL,
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    completed_at timestamptz,
    updated_at timestamptz NOT NULL
);
CREATE INDEX operations_project_created_idx ON secondbox.operations (tenant_ref, subject_ref, created_at, id);
CREATE INDEX operations_state_updated_idx ON secondbox.operations (state, updated_at, id);

CREATE TABLE secondbox.idempotency_records (
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    operation text NOT NULL,
    target_id text NOT NULL,
    idempotency_key text NOT NULL,
    request_hash text NOT NULL,
    response_resource_id text NOT NULL,
    response_json jsonb,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX idempotency_records_scope_idx
    ON secondbox.idempotency_records (tenant_ref, subject_ref, operation, target_id, idempotency_key);
CREATE INDEX idempotency_records_expiry_idx ON secondbox.idempotency_records (expires_at);

CREATE TABLE secondbox.audit_events (
    id text PRIMARY KEY,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    actor_kind text NOT NULL,
    actor_id text NOT NULL,
    action text NOT NULL,
    resource_kind text NOT NULL,
    resource_id text NOT NULL,
    outcome text NOT NULL,
    request_id text NOT NULL,
    details_json jsonb NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX audit_events_project_created_idx ON secondbox.audit_events (tenant_ref, subject_ref, created_at DESC, id DESC);
CREATE INDEX audit_events_action_created_idx ON secondbox.audit_events (action, created_at DESC, id DESC);

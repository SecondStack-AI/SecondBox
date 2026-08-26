CREATE TABLE secondbox.tenants (
    ref text PRIMARY KEY,
    state text NOT NULL,
    allowed_profile_grants_json jsonb NOT NULL,
    allowed_application_scopes_json jsonb NOT NULL,
    aggregate_quota_json jsonb NOT NULL,
    expiry_policy_json jsonb NOT NULL,
    metadata_json jsonb NOT NULL,
    expires_at timestamptz,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE INDEX tenants_created_idx ON secondbox.tenants (created_at, ref);

CREATE TABLE secondbox.subjects (
    tenant_ref text NOT NULL,
    ref text NOT NULL,
    state text NOT NULL,
    cleanup_state text NOT NULL,
    quota_json jsonb NOT NULL,
    metadata_json jsonb NOT NULL,
    expires_at timestamptz,
    revision bigint NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_ref, ref)
);
CREATE INDEX subjects_tenant_created_idx ON secondbox.subjects (tenant_ref, created_at, ref);

CREATE TABLE secondbox.authority_identities (
    id text PRIMARY KEY,
    kind text NOT NULL,
    lookup_id text NOT NULL
);
CREATE UNIQUE INDEX authority_identities_lookup_idx
    ON secondbox.authority_identities (lookup_id);

CREATE TABLE secondbox.tenant_controller_authorities (
    id text PRIMARY KEY,
    lookup_id text NOT NULL,
    tenant_ref text NOT NULL,
    grant_name text NOT NULL,
    state text NOT NULL,
    metadata_json jsonb NOT NULL,
    expires_at timestamptz,
    revision bigint NOT NULL,
    token_verifier_sha256 bytea NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX tenant_controller_authorities_lookup_idx
    ON secondbox.tenant_controller_authorities (lookup_id);
CREATE INDEX tenant_controller_authorities_tenant_created_idx
    ON secondbox.tenant_controller_authorities (tenant_ref, created_at, id);

CREATE TABLE secondbox.application_authorities (
    id text PRIMARY KEY,
    lookup_id text NOT NULL,
    tenant_ref text NOT NULL,
    subject_ref text NOT NULL,
    state text NOT NULL,
    scopes_json jsonb NOT NULL,
    profile_grants_json jsonb NOT NULL,
    metadata_json jsonb NOT NULL,
    expires_at timestamptz,
    revision bigint NOT NULL,
    token_verifier_sha256 bytea NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);
CREATE UNIQUE INDEX application_authorities_lookup_idx
    ON secondbox.application_authorities (lookup_id);
CREATE INDEX application_authorities_tenant_subject_created_idx
    ON secondbox.application_authorities (tenant_ref, subject_ref, created_at, id);

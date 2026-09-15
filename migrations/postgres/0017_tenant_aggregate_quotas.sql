CREATE TABLE secondbox.tenant_quotas (
    tenant_ref text PRIMARY KEY,
    max_sandboxes bigint NOT NULL,
    max_active_instances bigint NOT NULL,
    max_cpu_millis bigint NOT NULL,
    max_memory_bytes bigint NOT NULL,
    max_snapshots bigint NOT NULL,
    max_port_sessions bigint NOT NULL,
    max_concurrent_operations bigint NOT NULL,
    max_active_subjects bigint NOT NULL,
    max_application_authorities bigint NOT NULL,
    updated_at timestamptz NOT NULL
);

INSERT INTO secondbox.tenant_quotas (
    tenant_ref,max_sandboxes,max_active_instances,max_cpu_millis,max_memory_bytes,
    max_snapshots,max_port_sessions,max_concurrent_operations,max_active_subjects,
    max_application_authorities,updated_at
)
SELECT ref,
       (aggregate_quota_json->>'maxSandboxes')::bigint,
       (aggregate_quota_json->>'maxActiveInstances')::bigint,
       (aggregate_quota_json->>'maxCpuMillis')::bigint,
       (aggregate_quota_json->>'maxMemoryBytes')::bigint,
       (aggregate_quota_json->>'maxSnapshots')::bigint,
       (aggregate_quota_json->>'maxPortSessions')::bigint,
       (aggregate_quota_json->>'maxConcurrentOperations')::bigint,
       (aggregate_quota_json->>'maxActiveSubjects')::bigint,
       (aggregate_quota_json->>'maxApplicationAuthorities')::bigint,
       updated_at
FROM secondbox.tenants;

INSERT INTO secondbox.subject_quotas (
    tenant_ref,subject_ref,max_sandboxes,max_active_instances,max_cpu_millis,
    max_memory_bytes,max_snapshots,max_port_sessions,max_concurrent_operations,updated_at
)
SELECT tenant_ref,ref,
       (quota_json->>'maxSandboxes')::bigint,
       (quota_json->>'maxActiveInstances')::bigint,
       (quota_json->>'maxCpuMillis')::bigint,
       (quota_json->>'maxMemoryBytes')::bigint,
       (quota_json->>'maxSnapshots')::bigint,
       (quota_json->>'maxPortSessions')::bigint,
       (quota_json->>'maxConcurrentOperations')::bigint,
       updated_at
FROM secondbox.subjects
ON CONFLICT (tenant_ref,subject_ref) DO UPDATE SET
    max_sandboxes=EXCLUDED.max_sandboxes,
    max_active_instances=EXCLUDED.max_active_instances,
    max_cpu_millis=EXCLUDED.max_cpu_millis,
    max_memory_bytes=EXCLUDED.max_memory_bytes,
    max_snapshots=EXCLUDED.max_snapshots,
    max_port_sessions=EXCLUDED.max_port_sessions,
    max_concurrent_operations=EXCLUDED.max_concurrent_operations,
    updated_at=EXCLUDED.updated_at;

CREATE FUNCTION secondbox.lock_quota_ledger_rows() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    locked_tenant_ref text;
    locked_subject_ref text;
BEGIN
    locked_tenant_ref := CASE WHEN TG_OP='DELETE' THEN OLD.tenant_ref ELSE NEW.tenant_ref END;
    locked_subject_ref := CASE WHEN TG_OP='DELETE' THEN OLD.subject_ref ELSE NEW.subject_ref END;

    PERFORM tenant_ref FROM secondbox.tenant_quotas
    WHERE tenant_ref=locked_tenant_ref
    FOR UPDATE;
    IF locked_subject_ref IS NOT NULL THEN
        PERFORM subject_ref FROM secondbox.subject_quotas
        WHERE tenant_ref=locked_tenant_ref AND subject_ref=locked_subject_ref
        FOR UPDATE;
    END IF;
    RETURN CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
END;
$$;

CREATE TRIGGER sandboxes_quota_ledger_lock
BEFORE INSERT OR DELETE OR UPDATE OF state,desired_state ON secondbox.sandboxes
FOR EACH ROW EXECUTE FUNCTION secondbox.lock_quota_ledger_rows();

CREATE TRIGGER snapshots_quota_ledger_lock
BEFORE INSERT OR DELETE OR UPDATE OF state ON secondbox.snapshots
FOR EACH ROW EXECUTE FUNCTION secondbox.lock_quota_ledger_rows();

CREATE TRIGGER port_sessions_quota_ledger_lock
BEFORE INSERT OR DELETE OR UPDATE OF state ON secondbox.port_sessions
FOR EACH ROW EXECUTE FUNCTION secondbox.lock_quota_ledger_rows();

CREATE TRIGGER data_plane_sessions_quota_ledger_lock
BEFORE INSERT OR DELETE OR UPDATE OF state ON secondbox.data_plane_sessions
FOR EACH ROW EXECUTE FUNCTION secondbox.lock_quota_ledger_rows();

CREATE TRIGGER application_authorities_quota_ledger_lock
BEFORE INSERT OR DELETE OR UPDATE OF state ON secondbox.application_authorities
FOR EACH ROW EXECUTE FUNCTION secondbox.lock_quota_ledger_rows();

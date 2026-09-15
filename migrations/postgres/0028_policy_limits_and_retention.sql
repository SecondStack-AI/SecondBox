ALTER TABLE secondbox.subject_quotas
    ALTER COLUMN max_sandboxes DROP NOT NULL,
    ALTER COLUMN max_active_instances DROP NOT NULL,
    ALTER COLUMN max_cpu_millis DROP NOT NULL,
    ALTER COLUMN max_memory_bytes DROP NOT NULL,
    ALTER COLUMN max_snapshots DROP NOT NULL,
    ALTER COLUMN max_port_sessions DROP NOT NULL,
    ALTER COLUMN max_concurrent_operations DROP NOT NULL;

ALTER TABLE secondbox.tenant_quotas
    ALTER COLUMN max_sandboxes DROP NOT NULL,
    ALTER COLUMN max_active_instances DROP NOT NULL,
    ALTER COLUMN max_cpu_millis DROP NOT NULL,
    ALTER COLUMN max_memory_bytes DROP NOT NULL,
    ALTER COLUMN max_snapshots DROP NOT NULL,
    ALTER COLUMN max_port_sessions DROP NOT NULL,
    ALTER COLUMN max_concurrent_operations DROP NOT NULL,
    ALTER COLUMN max_active_subjects DROP NOT NULL,
    ALTER COLUMN max_application_authorities DROP NOT NULL;

ALTER TABLE secondbox.sandboxes ADD COLUMN lifecycle_policy_json jsonb;
ALTER TABLE secondbox.subjects ADD COLUMN sandbox_policy_json jsonb;
ALTER TABLE secondbox.snapshots ALTER COLUMN retain_until DROP NOT NULL;

-- The CPU dimension of every persisted contract changes representation from
-- CPU milli-units to whole vCPUs, and RunnerPools and Runners gain a persisted
-- compute-backend identity. The baseline stays immutable; this migration
-- converts an existing database to exactly what a fresh deployment records.
--
-- Milli-unit values convert by rounding up to the whole vCPU that already
-- covered the stated allowance. Quota and Profile values additionally floor at
-- one vCPU so no subject or revision converts to an unschedulable zero: the
-- conversion may only widen an allowance, never strand recorded usage above a
-- shrunken limit.
ALTER TABLE secondbox.subject_quotas RENAME COLUMN max_cpu_millis TO max_vcpu_count;
UPDATE secondbox.subject_quotas
SET max_vcpu_count = GREATEST(1, max_vcpu_count / 1000 + CASE WHEN max_vcpu_count % 1000 > 0 THEN 1 ELSE 0 END);

-- Tenant aggregate quotas bound the same CPU dimension one level up and
-- convert identically so a tenant ceiling keeps covering every subject
-- allowance it covered before.
ALTER TABLE secondbox.tenant_quotas RENAME COLUMN max_cpu_millis TO max_vcpu_count;
UPDATE secondbox.tenant_quotas
SET max_vcpu_count = GREATEST(1, max_vcpu_count / 1000 + CASE WHEN max_vcpu_count % 1000 > 0 THEN 1 ELSE 0 END);

-- The Tenant row keeps its own copy of the aggregate quota document, which is
-- decoded on every read, so its recorded key converts with the ledger.
UPDATE secondbox.tenants
SET aggregate_quota_json = jsonb_set(
        aggregate_quota_json - 'maxCpuMillis',
        '{maxVcpuCount}',
        to_jsonb(GREATEST(1,
            ((aggregate_quota_json->>'maxCpuMillis')::bigint) / 1000
            + CASE WHEN ((aggregate_quota_json->>'maxCpuMillis')::bigint) % 1000 > 0 THEN 1 ELSE 0 END)))
WHERE aggregate_quota_json ? 'maxCpuMillis';

-- Subject rows keep the same decoded-quota copy and convert the same way.
UPDATE secondbox.subjects
SET quota_json = jsonb_set(
        quota_json - 'maxCpuMillis',
        '{maxVcpuCount}',
        to_jsonb(GREATEST(1,
            ((quota_json->>'maxCpuMillis')::bigint) / 1000
            + CASE WHEN ((quota_json->>'maxCpuMillis')::bigint) % 1000 > 0 THEN 1 ELSE 0 END)))
WHERE quota_json ? 'maxCpuMillis';

-- Profile revisions are immutable statements of intent; rewriting the unit is
-- a representation change of the same allowance, not a new policy. The guard
-- keeps this idempotent and converges an upgraded database on exactly the
-- spec_json a fresh one writes.
UPDATE secondbox.profile_revisions
SET spec_json = jsonb_set(
        spec_json #- '{resources,cpuMillis}',
        '{resources,vcpuCount}',
        to_jsonb(GREATEST(1,
            ((spec_json->'resources'->>'cpuMillis')::bigint) / 1000
            + CASE WHEN ((spec_json->'resources'->>'cpuMillis')::bigint) % 1000 > 0 THEN 1 ELSE 0 END)))
WHERE spec_json->'resources' ? 'cpuMillis';

-- The Profile process limit is removed everywhere: guest process counts are
-- not host PIDs and the backend owns its host PID ceiling, so a retained
-- legacy field would state a promise no fresh deployment makes.
UPDATE secondbox.profile_revisions
SET spec_json = spec_json #- '{resources,processLimit}'
WHERE spec_json->'resources' ? 'processLimit';

-- Runner capacity documents marshal with Go field names. Advertised capacity
-- is replaced on the next enrollment, but reserved capacity survives a
-- coordinated upgrade with in-flight reservations, so both convert here. No
-- floor applies: a zero reservation stays zero.
UPDATE secondbox.runners
SET capacity_json = jsonb_set(
        capacity_json - 'CPUMillis',
        '{VCPUCount}',
        to_jsonb(((capacity_json->>'CPUMillis')::bigint) / 1000
            + CASE WHEN ((capacity_json->>'CPUMillis')::bigint) % 1000 > 0 THEN 1 ELSE 0 END))
WHERE capacity_json ? 'CPUMillis';
UPDATE secondbox.runners
SET reserved_capacity_json = jsonb_set(
        reserved_capacity_json - 'CPUMillis',
        '{VCPUCount}',
        to_jsonb(((reserved_capacity_json->>'CPUMillis')::bigint) / 1000
            + CASE WHEN ((reserved_capacity_json->>'CPUMillis')::bigint) % 1000 > 0 THEN 1 ELSE 0 END))
WHERE reserved_capacity_json ? 'CPUMillis';

-- RunnerPool capacity policies recorded the same milli-unit key.
UPDATE secondbox.runner_pools
SET capacity_policy_json = jsonb_set(
        capacity_policy_json - 'maxCpuMillis',
        '{maxVcpuCount}',
        to_jsonb(((capacity_policy_json->>'maxCpuMillis')::bigint) / 1000
            + CASE WHEN ((capacity_policy_json->>'maxCpuMillis')::bigint) % 1000 > 0 THEN 1 ELSE 0 END))
WHERE capacity_policy_json ? 'maxCpuMillis';

-- Every RunnerPool and Runner records which compute backend it serves. The
-- empty default means "not yet sealed": existing pools seal to a backend kind
-- on the first post-upgrade advertisement, exactly like a fresh pool.
ALTER TABLE secondbox.runner_pools ADD COLUMN backend_kind text NOT NULL DEFAULT '';
ALTER TABLE secondbox.runners ADD COLUMN backend_kind text NOT NULL DEFAULT '';

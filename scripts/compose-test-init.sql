INSERT INTO secondbox.runner_pools (
    name,
    state,
    architectures_json,
    capabilities_json,
    capacity_policy_json,
    ready_runner_count,
    revision,
    created_at,
    updated_at
) VALUES (
    'compose-live-pool',
    'ready',
    '["amd64"]'::jsonb,
    '["firecracker"]'::jsonb,
    '{}'::jsonb,
    1,
    1,
    '2026-07-28T00:00:00Z',
    '2026-07-28T00:00:00Z'
);

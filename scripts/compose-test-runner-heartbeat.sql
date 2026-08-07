-- The Compose fixture Runner is a database row, not a connected Runner, so
-- nothing renews its heartbeat. The control plane's Runner-loss reconciler marks
-- any Runner offline once last_seen_at falls behind
-- SECONDBOX_RUNNER_HEARTBEAT_TIMEOUT_MILLISECONDS, and an offline Runner is not a
-- placement candidate, so every live half restates the fixture's liveness
-- immediately before it runs.
UPDATE secondbox.runners
SET state = 'ready',
    last_seen_at = CURRENT_TIMESTAMP,
    revision = revision + 1,
    updated_at = CURRENT_TIMESTAMP
WHERE id = 'compose-live-runner'
RETURNING id;

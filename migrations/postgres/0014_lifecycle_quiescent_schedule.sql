-- The lifecycle claim scan no longer restates the reconciler's rest matrix.
-- A Sandbox the reconciler left at rest parks its schedule instead, so the
-- reconciliation work set is exactly the Sandboxes carrying a next_reconcile_at
-- and the index no longer holds an entry per resting Sandbox.
DROP INDEX secondbox.sandboxes_lifecycle_reconcile_idx;

CREATE INDEX sandboxes_lifecycle_reconcile_idx
    ON secondbox.sandboxes (next_reconcile_at, id)
    WHERE next_reconcile_at IS NOT NULL
      AND state<>'deleted'
      AND NOT (state='failed' AND lifecycle_failure_class<>'');

-- Sandboxes that reached rest before this migration kept a stale past deadline
-- that only the removed predicate suppressed. Park them here so the upgraded
-- population converges on the same schedule a fresh one commits, instead of
-- paying one claim, decision, and commit each on the first poll after upgrade.
UPDATE secondbox.sandboxes
SET next_reconcile_at=NULL
WHERE next_reconcile_at IS NOT NULL
  AND (
    (desired_state='stopped' AND state IN ('stopped','failed'))
    OR (desired_state='deleted' AND state='deleted')
  );

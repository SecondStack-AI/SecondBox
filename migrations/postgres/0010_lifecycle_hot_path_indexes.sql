CREATE INDEX sandboxes_lifecycle_reconcile_idx
    ON secondbox.sandboxes (next_reconcile_at, id)
    WHERE state<>'deleted'
      AND NOT (state='failed' AND lifecycle_failure_class<>'')
      AND NOT (state IN ('stopped','failed') AND desired_state='stopped');

CREATE INDEX leases_active_expiry_idx
    ON secondbox.leases (expires_at, id)
    WHERE state='active';

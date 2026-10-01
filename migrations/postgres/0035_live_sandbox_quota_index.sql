-- Quota admission counts a Subject's and a Tenant's live Sandboxes while it
-- holds their quota ledgers. Deleted Sandboxes are retained, so without this
-- index every admission scanned the whole retained history under the lock.
CREATE INDEX sandboxes_live_quota_idx
    ON secondbox.sandboxes (tenant_ref, subject_ref)
    INCLUDE (state, desired_state, vcpu_count, memory_bytes, workspace_id)
    WHERE state<>'deleted';

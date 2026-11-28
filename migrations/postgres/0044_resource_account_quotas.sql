-- A failed Instance keeps its charge until its generation is fenced. A failed
-- start that never placed has no current Instance and releases immediately.
CREATE FUNCTION secondbox.sandbox_quota_active(state text, desired_state text, current_instance_id text)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
    SELECT secondbox.sandbox_quota_active(state,desired_state)
        OR (state='failed' AND current_instance_id<>'')
$$;

CREATE OR REPLACE FUNCTION secondbox.lock_sandbox_quota_ledger_rows() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    sandbox secondbox.sandboxes;
BEGIN
    sandbox := CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
    IF TG_OP='UPDATE' THEN
        IF NOT (
            (OLD.state='deleted' AND NEW.state<>'deleted')
            OR (secondbox.sandbox_quota_active(NEW.state,NEW.desired_state,NEW.current_instance_id)
                AND NOT secondbox.sandbox_quota_active(OLD.state,OLD.desired_state,OLD.current_instance_id))
        ) THEN
            RETURN NEW;
        END IF;
        -- A caller that locked this Sandbox without its ledgers would take
        -- them here, after the Sandbox row, inverting the lock order.
        IF position(','||NEW.id||',' IN COALESCE(current_setting('secondbox.ledgerless_sandboxes', true),'')) > 0 THEN
            RAISE EXCEPTION 'SecondBox Sandbox % increases quota usage without its quota ledger lock', NEW.id;
        END IF;
    END IF;
    PERFORM tenant_ref FROM secondbox.tenant_quotas
    WHERE tenant_ref=sandbox.tenant_ref
    FOR UPDATE;
    PERFORM subject_ref FROM secondbox.subject_quotas
    WHERE tenant_ref=sandbox.tenant_ref AND subject_ref=sandbox.subject_ref
    FOR UPDATE;
    RETURN sandbox;
END;
$$;

DROP TRIGGER sandboxes_quota_ledger_lock ON secondbox.sandboxes;

CREATE TRIGGER sandboxes_quota_ledger_lock
BEFORE INSERT OR DELETE OR UPDATE OF state,desired_state,current_instance_id ON secondbox.sandboxes
FOR EACH ROW EXECUTE FUNCTION secondbox.lock_sandbox_quota_ledger_rows();

ALTER TABLE secondbox.sandboxes ADD COLUMN resource_account_key text;
CREATE INDEX sandboxes_resource_account_active ON secondbox.sandboxes
    (tenant_ref,subject_ref,resource_account_key)
    WHERE resource_account_key IS NOT NULL AND secondbox.sandbox_quota_active(state,desired_state,current_instance_id);

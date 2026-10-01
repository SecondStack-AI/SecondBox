-- Quota admission counts usage while it holds the quota ledgers, so only a
-- Sandbox write that increases counted usage must serialize with it. A
-- transition that keeps or reduces usage can at worst make a concurrent
-- admission count conservatively, and no longer takes the ledgers.
CREATE FUNCTION secondbox.sandbox_quota_active(state text, desired_state text)
RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (desired_state='running' AND state IN ('creating','stopped'))
        OR state IN ('starting','ready','draining','stopping')
$$;

CREATE FUNCTION secondbox.lock_sandbox_quota_ledger_rows() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    sandbox secondbox.sandboxes;
BEGIN
    sandbox := CASE WHEN TG_OP='DELETE' THEN OLD ELSE NEW END;
    IF TG_OP='UPDATE' THEN
        IF NOT (
            (OLD.state='deleted' AND NEW.state<>'deleted')
            OR (secondbox.sandbox_quota_active(NEW.state,NEW.desired_state)
                AND NOT secondbox.sandbox_quota_active(OLD.state,OLD.desired_state))
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
BEFORE INSERT OR DELETE OR UPDATE OF state,desired_state ON secondbox.sandboxes
FOR EACH ROW EXECUTE FUNCTION secondbox.lock_sandbox_quota_ledger_rows();

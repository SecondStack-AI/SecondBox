CREATE TABLE secondbox.operation_stage_timings (
    operation_id text NOT NULL,
    sandbox_id text NOT NULL,
    stage text NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (operation_id, stage)
);

CREATE INDEX operation_stage_timings_sandbox_observed_idx
    ON secondbox.operation_stage_timings (sandbox_id, observed_at, operation_id);

CREATE FUNCTION secondbox.notify_runner_command_work()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state = 'pending' THEN
            PERFORM pg_notify(
                'secondbox_work',
                json_build_object('kind', 'runner_command', 'key', NEW.runner_id)::text
            );
        END IF;
    ELSIF NEW.state = 'pending' AND OLD.state IS DISTINCT FROM NEW.state THEN
        PERFORM pg_notify(
            'secondbox_work',
            json_build_object('kind', 'runner_command', 'key', NEW.runner_id)::text
        );
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER runner_commands_notify_pending
AFTER INSERT OR UPDATE OF state ON secondbox.runner_commands
FOR EACH ROW
EXECUTE FUNCTION secondbox.notify_runner_command_work();

CREATE FUNCTION secondbox.notify_lifecycle_work()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.next_reconcile_at IS NOT NULL
       AND NEW.next_reconcile_at <= clock_timestamp() THEN
        IF TG_OP = 'INSERT' THEN
            PERFORM pg_notify(
                'secondbox_work',
                json_build_object('kind', 'lifecycle', 'key', '')::text
            );
        ELSIF OLD.next_reconcile_at IS DISTINCT FROM NEW.next_reconcile_at THEN
            PERFORM pg_notify(
                'secondbox_work',
                json_build_object('kind', 'lifecycle', 'key', '')::text
            );
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER sandboxes_notify_due
AFTER INSERT OR UPDATE OF next_reconcile_at ON secondbox.sandboxes
FOR EACH ROW
EXECUTE FUNCTION secondbox.notify_lifecycle_work();

CREATE FUNCTION secondbox.notify_assignment_work()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.next_reconcile_at IS NOT NULL
       AND NEW.next_reconcile_at <= clock_timestamp() THEN
        IF TG_OP = 'INSERT' THEN
            PERFORM pg_notify(
                'secondbox_work',
                json_build_object('kind', 'assignment', 'key', '')::text
            );
        ELSIF OLD.next_reconcile_at IS DISTINCT FROM NEW.next_reconcile_at THEN
            PERFORM pg_notify(
                'secondbox_work',
                json_build_object('kind', 'assignment', 'key', '')::text
            );
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER assignments_notify_due
AFTER INSERT OR UPDATE OF next_reconcile_at ON secondbox.assignments
FOR EACH ROW
EXECUTE FUNCTION secondbox.notify_assignment_work();

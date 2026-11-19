CREATE OR REPLACE FUNCTION secondbox.notify_runner_command_work()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- Unplaced creates are durable demand, not work for an anonymous Runner.
    IF NEW.state <> 'pending' OR NEW.runner_id = '' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'INSERT' THEN
        PERFORM pg_notify(
            'secondbox_work',
            json_build_object('kind', 'runner_command', 'key', NEW.runner_id)::text
        );
    ELSIF OLD.state IS DISTINCT FROM NEW.state OR OLD.runner_id IS DISTINCT FROM NEW.runner_id THEN
        PERFORM pg_notify(
            'secondbox_work',
            json_build_object('kind', 'runner_command', 'key', NEW.runner_id)::text
        );
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER runner_commands_notify_pending ON secondbox.runner_commands;
CREATE TRIGGER runner_commands_notify_pending
AFTER INSERT OR UPDATE OF state, runner_id ON secondbox.runner_commands
FOR EACH ROW
EXECUTE FUNCTION secondbox.notify_runner_command_work();

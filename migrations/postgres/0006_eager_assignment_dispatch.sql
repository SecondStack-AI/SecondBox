CREATE OR REPLACE FUNCTION secondbox.notify_runner_command_work()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state IN ('pending', 'delivering') THEN
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

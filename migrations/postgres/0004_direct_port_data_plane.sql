ALTER TABLE secondbox.runners
    ADD COLUMN data_plane_address text NOT NULL DEFAULT '';

ALTER TABLE secondbox.port_sessions
    ADD COLUMN transport text NOT NULL DEFAULT 'relay',
    ADD COLUMN credential_digest bytea NOT NULL DEFAULT ''::bytea;

-- A direct PortSession's admitting frame is on the caller's connect path: the
-- Runner cannot admit a caller connection until it arrives, so waiting for the
-- outbound poll interval put that interval back into connect time. Waking the
-- runner command pump on commit removes it. The durable frame remains the only
-- authority; the notification is a hint and the existing poll remains the
-- fallback.
--
-- The scope is deliberately one row per direct PortSession admission. NOTIFY has
-- no server-side filtering, so every payload reaches every listening replica and
-- all but one discard it. A trigger on data_plane_frames would broadcast once
-- per relay Port message, per Exec and PTY stdin chunk, and per File chunk, and
-- would charge each of those inserts a session lookup. This plan moves only the
-- Port byte path, so the wakeup must not silently become an unmeasured change
-- for the transports it leaves on the relay.
--
-- Admitting a direct PortSession is therefore the trigger event: it happens once
-- per session, in the same transaction that enqueued the admitting frame, and it
-- names the transport directly instead of inferring it. Relay Port, Exec, PTY,
-- and File inserts execute nothing.
--
-- A later frame to a direct session, such as an application-requested cancel,
-- still reaches the Runner on the existing poll. That is unchanged behaviour: a
-- live direct connection is torn down locally by fence, Lease expiry, deadline,
-- drain, Instance termination, and control-connection loss, none of which depend
-- on this notification.
CREATE FUNCTION secondbox.notify_direct_port_admission()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    target_runner_id text;
BEGIN
    SELECT session.runner_id INTO target_runner_id
    FROM secondbox.data_plane_sessions AS session
    WHERE session.id = NEW.data_plane_session_id;
    IF target_runner_id IS NOT NULL AND target_runner_id <> '' THEN
        PERFORM pg_notify(
            'secondbox_work',
            json_build_object('kind', 'runner_command', 'key', target_runner_id)::text
        );
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER port_sessions_notify_direct_admission
AFTER INSERT ON secondbox.port_sessions
FOR EACH ROW
WHEN (NEW.transport = 'direct')
EXECUTE FUNCTION secondbox.notify_direct_port_admission();

-- Relay data-plane delivery waited for a poll interval in both directions. The
-- durable frame remains the only authority; these notifications are hints and
-- the configured poll intervals remain the recovery bound.
--
-- The two directions are not symmetric in this schema, so they do not get the
-- same rule. Outbound frames are inserted 'pending' and claimed by the runner
-- command pump, so the durable rows record whether the consumer is behind.
-- Inbound frames are inserted already 'delivered' and are read by cursor, so no
-- durable row records how far a caller has read.

-- Outbound fires only on the idle-to-pending transition. Consumers drain until
-- the authoritative query reports no work, so a notification delivered while a
-- consumer is already draining changes nothing. A burst of stdin or File-upload
-- frames therefore costs one notification rather than one per frame, and the
-- session lookup that resolves the runner runs only on that transition.
--
-- Two concurrent inserts in separate transactions cannot observe each other, so
-- both may notify. That duplicate is harmless: the hub coalesces and the
-- consumer drains.
CREATE FUNCTION secondbox.notify_outbound_relay_work()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    target_runner_id text;
BEGIN
    IF EXISTS (
        SELECT 1 FROM secondbox.data_plane_frames AS pending
        WHERE pending.session_id = NEW.session_id
          AND pending.direction = 'outbound'
          AND pending.state = 'pending'
          AND pending.id <> NEW.id
    ) THEN
        RETURN NEW;
    END IF;
    SELECT session.runner_id INTO target_runner_id
    FROM secondbox.data_plane_sessions AS session
    WHERE session.id = NEW.session_id;
    IF target_runner_id IS NOT NULL AND target_runner_id <> '' THEN
        PERFORM pg_notify(
            'secondbox_work',
            json_build_object('kind', 'runner_command', 'key', target_runner_id)::text
        );
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER data_plane_frames_notify_outbound
AFTER INSERT ON secondbox.data_plane_frames
FOR EACH ROW
WHEN (NEW.direction = 'outbound' AND NEW.state = 'pending')
EXECUTE FUNCTION secondbox.notify_outbound_relay_work();

-- Inbound notifies per frame, deliberately. Collapsing would require a durable
-- consumer cursor, and adding one would put a write on the guest-output path to
-- save a notification. The key is already on the row, so there is no lookup,
-- and every inbound frame is output an attached caller is already waiting for.
CREATE FUNCTION secondbox.notify_inbound_relay_work()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM pg_notify(
        'secondbox_work',
        json_build_object('kind', 'data_plane_session', 'key', NEW.session_id)::text
    );
    RETURN NEW;
END;
$$;

CREATE TRIGGER data_plane_frames_notify_inbound
AFTER INSERT ON secondbox.data_plane_frames
FOR EACH ROW
WHEN (NEW.direction = 'inbound')
EXECUTE FUNCTION secondbox.notify_inbound_relay_work();

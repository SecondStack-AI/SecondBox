ALTER TABLE secondbox.data_plane_sessions
    ADD COLUMN result_json jsonb;

UPDATE secondbox.data_plane_sessions
SET result_json=jsonb_build_object(
    'stdout',encode(stdout_bytes,'base64'),
    'stderr',encode(stderr_bytes,'base64'),
    'content',encode(content_bytes,'base64')
);

ALTER TABLE secondbox.data_plane_sessions
    ALTER COLUMN result_json SET NOT NULL;

DROP TABLE secondbox.data_plane_frames;
DROP FUNCTION secondbox.notify_outbound_relay_work();
DROP FUNCTION secondbox.notify_inbound_relay_work();

ALTER TABLE secondbox.data_plane_sessions
    DROP COLUMN stdout_bytes,
    DROP COLUMN stderr_bytes,
    DROP COLUMN content_bytes,
    DROP COLUMN frames_retain_until,
    DROP COLUMN frame_cleanup_completed_at,
    DROP COLUMN terminal_inbound_sequence,
    DROP COLUMN terminal_inbound_payload_hash;

-- Relay frames are delivery and replay material, while the session row also
-- owns materialised results and admission idempotency.  Keep their horizons
-- separate so payload can be removed as soon as its final consumer is gone.
ALTER TABLE secondbox.data_plane_sessions
    ADD COLUMN frames_retain_until timestamptz,
    ADD COLUMN frame_cleanup_completed_at timestamptz,
    ADD COLUMN next_outbound_sequence bigint,
    ADD COLUMN terminal_inbound_sequence bigint,
    ADD COLUMN terminal_inbound_payload_hash text;

UPDATE secondbox.data_plane_sessions AS session
SET frames_retain_until=session.retain_until,
    next_outbound_sequence=COALESCE((
        SELECT max(frame.sequence)+1
        FROM secondbox.data_plane_frames AS frame
        WHERE frame.session_id=session.id AND frame.direction='outbound'
    ),1),
    terminal_inbound_sequence=CASE
        WHEN session.state IN ('completed','failed','cancelled','expired')
        THEN session.next_inbound_sequence-1
        ELSE NULL
    END,
    terminal_inbound_payload_hash=CASE
        WHEN session.state IN ('completed','failed','cancelled','expired')
        THEN (
            SELECT frame.payload_hash
            FROM secondbox.data_plane_frames AS frame
            WHERE frame.session_id=session.id
              AND frame.direction='inbound'
              AND frame.sequence=session.next_inbound_sequence-1
        )
        ELSE NULL
    END;

ALTER TABLE secondbox.data_plane_sessions
    ALTER COLUMN frames_retain_until SET NOT NULL,
    ALTER COLUMN next_outbound_sequence SET NOT NULL;

ALTER TABLE secondbox.port_sessions
    ADD COLUMN acknowledged_inbound_sequence bigint;

UPDATE secondbox.port_sessions AS port
SET acknowledged_inbound_sequence=COALESCE((
    SELECT max(frame.sequence)
    FROM secondbox.data_plane_frames AS frame
    WHERE frame.session_id=port.data_plane_session_id
      AND frame.direction='inbound'
      AND frame.consumed_at IS NOT NULL
),0);

ALTER TABLE secondbox.port_sessions
    ALTER COLUMN acknowledged_inbound_sequence SET NOT NULL;

CREATE INDEX data_plane_sessions_frame_retention_idx
    ON secondbox.data_plane_sessions (frames_retain_until,state,id)
    WHERE frame_cleanup_completed_at IS NULL;

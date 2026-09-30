-- Attribution binds one admitted exec inside an ordinary generation, not an
-- Assignment. The session keeps the binding for replay and Runner dispatch.
ALTER TABLE secondbox.assignments
    DROP COLUMN IF EXISTS execution_authorization_ref,
    DROP COLUMN IF EXISTS execution_expires_at,
    DROP COLUMN IF EXISTS execution_session_id;

ALTER TABLE secondbox.data_plane_sessions
    ADD COLUMN execution_authorization_ref text,
    ADD COLUMN execution_expires_at timestamptz;

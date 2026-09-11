ALTER TABLE secondbox.assignments
    ADD COLUMN execution_authorization_ref text,
    ADD COLUMN execution_expires_at timestamptz,
    ADD COLUMN execution_session_id text;

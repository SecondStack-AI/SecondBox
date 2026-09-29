-- A Port session's traffic lives only in the process that relays it. Its byte
-- counts were recorded for a per-session byte limit that Port sessions no
-- longer carry, and nothing else reads them.
ALTER TABLE secondbox.port_sessions
    DROP COLUMN IF EXISTS client_bytes,
    DROP COLUMN IF EXISTS runner_bytes;

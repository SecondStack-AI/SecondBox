-- Proxied Port credit and acknowledgement are enforced in memory by the tunnel
-- that owns the stream; PostgreSQL records only periodic byte counts and the
-- terminal outcome. The per-chunk credit balance and acknowledgement cursor no
-- longer describe anything, and a single-use tunnel never resumes from them.
ALTER TABLE secondbox.port_sessions
    DROP COLUMN IF EXISTS client_credit_bytes,
    DROP COLUMN IF EXISTS acknowledged_inbound_sequence;

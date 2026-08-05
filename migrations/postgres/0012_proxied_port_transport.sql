UPDATE secondbox.port_sessions
SET transport = 'proxied'
WHERE transport = 'relay';

ALTER TABLE secondbox.port_sessions
    ALTER COLUMN transport SET DEFAULT 'proxied';

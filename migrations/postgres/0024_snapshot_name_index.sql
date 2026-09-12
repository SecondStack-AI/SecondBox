-- A ready Snapshot name resolves to one retained disk state per Sandbox.
DO $$
DECLARE
    conflicting text;
BEGIN
    SELECT string_agg(format('%s/%s', sandbox_id, name), ', ' ORDER BY sandbox_id, name)
      INTO conflicting
      FROM (
          SELECT sandbox_id, name
            FROM secondbox.snapshots
           WHERE state = 'ready'
           GROUP BY sandbox_id, name
          HAVING count(*) > 1
      ) AS collisions;
    IF conflicting IS NOT NULL THEN
        RAISE EXCEPTION
            'SecondBox Snapshot name is held by more than one ready Snapshot: %. Delete duplicates by Snapshot identifier before upgrading.',
            conflicting;
    END IF;
END
$$;

CREATE UNIQUE INDEX snapshots_sandbox_ready_name_idx
    ON secondbox.snapshots (sandbox_id, name)
    WHERE state = 'ready';

-- Supports the Sandbox metadata filter on listSandboxes, and makes the reserved
-- name key resolve to exactly one live Sandbox per tenant and subject.
--
-- The reserved key is ordinary caller-writable Metadata, so a database written
-- before this migration may already hold a duplicate. Report that as an
-- actionable message rather than letting index creation fail with a raw unique
-- violation: migrations run before listeners open, so the difference is between
-- an operator knowing which Sandboxes to rename and a control plane that will
-- not start.
DO $$
DECLARE
    conflicting text;
BEGIN
    SELECT string_agg(
               format('%s/%s=%s', tenant_ref, subject_ref, reserved_name),
               ', '
               ORDER BY tenant_ref, subject_ref, reserved_name
           )
      INTO conflicting
      FROM (
          SELECT tenant_ref,
                 subject_ref,
                 metadata_json->>'secondbox.dev/name' AS reserved_name
            FROM secondbox.sandboxes
           WHERE (metadata_json->>'secondbox.dev/name') IS NOT NULL
             AND deleted_at IS NULL
           GROUP BY tenant_ref, subject_ref, metadata_json->>'secondbox.dev/name'
          HAVING count(*) > 1
      ) AS collisions;

    IF conflicting IS NOT NULL THEN
        RAISE EXCEPTION
            'SecondBox reserved Sandbox name is held by more than one live Sandbox: %. Rename or delete the duplicates before upgrading.',
            conflicting;
    END IF;
END
$$;

CREATE INDEX sandboxes_metadata_idx
    ON secondbox.sandboxes USING gin (metadata_json jsonb_path_ops);

CREATE UNIQUE INDEX sandboxes_subject_name_idx
    ON secondbox.sandboxes (
        tenant_ref,
        subject_ref,
        (metadata_json->>'secondbox.dev/name')
    )
    WHERE (metadata_json->>'secondbox.dev/name') IS NOT NULL
      AND deleted_at IS NULL;

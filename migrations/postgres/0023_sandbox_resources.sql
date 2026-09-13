-- A Sandbox pins its allocation independently of subsequent Profile revisions.
ALTER TABLE secondbox.sandboxes
    ADD COLUMN vcpu_count bigint,
    ADD COLUMN memory_bytes bigint,
    ADD COLUMN workspace_bytes bigint;

UPDATE secondbox.sandboxes AS sandbox
SET vcpu_count = (revision.spec_json->'resources'->>'vcpuCount')::bigint,
    memory_bytes = (revision.spec_json->'resources'->>'memoryBytes')::bigint,
    workspace_bytes = (revision.spec_json->'resources'->>'workspaceBytes')::bigint
FROM secondbox.profile_revisions AS revision
WHERE revision.id = sandbox.profile_revision_id;

ALTER TABLE secondbox.sandboxes
    ALTER COLUMN vcpu_count SET NOT NULL,
    ALTER COLUMN memory_bytes SET NOT NULL,
    ALTER COLUMN workspace_bytes SET NOT NULL;

ALTER TABLE secondbox.sandboxes
    ADD COLUMN execution_image_reference text NOT NULL DEFAULT '',
    ADD COLUMN execution_image_digest text NOT NULL DEFAULT '';

UPDATE secondbox.sandboxes AS sandbox
SET execution_image_reference=image.requested_image_reference,
    execution_image_digest=image.resolved_image_digest
FROM (
    SELECT DISTINCT ON (sandbox_id) sandbox_id,requested_image_reference,resolved_image_digest
    FROM secondbox.instances
    WHERE resolved_image_digest <> ''
    ORDER BY sandbox_id,created_at DESC,id DESC
) AS image
WHERE image.sandbox_id=sandbox.id;

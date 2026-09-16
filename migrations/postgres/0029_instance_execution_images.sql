ALTER TABLE secondbox.instances
    ADD COLUMN requested_image_reference text NOT NULL DEFAULT '',
    ADD COLUMN resolved_image_digest text NOT NULL DEFAULT '';

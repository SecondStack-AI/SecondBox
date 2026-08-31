ALTER TABLE secondbox.runners
    ADD COLUMN supported_egress_contexts_json jsonb NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE secondbox.assignments
    ADD COLUMN egress_context text;

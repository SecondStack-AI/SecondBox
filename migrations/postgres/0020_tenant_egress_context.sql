ALTER TABLE secondbox.tenants
    ADD COLUMN egress_context text;

ALTER TABLE secondbox.sandboxes
    ADD COLUMN egress_context text;

ALTER TABLE secondbox.workspace_relocations
    ADD COLUMN egress_context text;

-- A pending start may own independent export-cleanup and restore phases.
-- Receipts are authorized by their exact Runner command, not Operation alone.
DROP INDEX secondbox.workspace_archives_operation_idx;
CREATE INDEX workspace_archives_operation_idx ON secondbox.workspace_archives(operation_id);

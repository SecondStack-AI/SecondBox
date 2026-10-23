-- A start may bound how long it waits unplaced. The reconciler fails a start
-- Operation still unplaced at this instant with capacity_wait_expired.
ALTER TABLE secondbox.operations ADD COLUMN capacity_wait_deadline timestamptz;

-- Every Profile revision now states how its Instances reach ready. An operator
-- states startup.mode explicitly on every new revision and the control plane has
-- no application default, so a revision written before this migration carries no
-- mode at all and would decode as an empty one.
--
-- Revisions that predate the field ran exactly one startup path: they booted the
-- guest. Stamping them cold_boot is therefore a statement of the behavior they
-- already have, not a default being invented for them. Immutability is preserved
-- in the sense that matters: the recorded policy still describes what every
-- Sandbox pinned to that revision has always done.
--
-- The guard keeps this idempotent and keeps it from touching a revision that
-- already states a mode, so an upgraded database converges on exactly the
-- spec_json a fresh one writes.
UPDATE secondbox.profile_revisions
SET spec_json = jsonb_set(spec_json, '{startup}', '{"mode": "cold_boot"}'::jsonb, true)
WHERE NOT (spec_json ? 'startup');

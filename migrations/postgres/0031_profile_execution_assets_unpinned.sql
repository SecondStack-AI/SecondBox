-- Profile revisions no longer name execution bundle digests. An Instance boots
-- the signed bundle its home Runner has installed, so a Sandbox created under an
-- earlier release keeps its Workspace and starts on whatever release the Runner
-- now runs.
--
-- Removing the keys states the policy those revisions now have rather than
-- rewriting it: the bundle digests stop describing anything a Sandbox pinned to
-- the revision will ever do. The guard keeps this idempotent, so an upgraded
-- database converges on exactly the spec_json a fresh one writes.
UPDATE secondbox.profile_revisions
SET spec_json = spec_json - 'runtimeBundleDigest' - 'toolchainBundleDigest'
WHERE spec_json ?| ARRAY['runtimeBundleDigest', 'toolchainBundleDigest'];

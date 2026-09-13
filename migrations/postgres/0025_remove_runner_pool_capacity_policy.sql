-- RunnerPool capacity policy was descriptive metadata, never an admission
-- limit. Tenant/Subject quotas and reported Runner capacity own admission.
ALTER TABLE secondbox.runner_pools DROP COLUMN capacity_policy_json;

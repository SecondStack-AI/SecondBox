-- A Subject Sandbox policy names a set of Profiles. Rewrite each stored
-- single-Profile selection as a one-element set; values are otherwise unchanged.
UPDATE secondbox.subjects
SET sandbox_policy_json = (sandbox_policy_json - 'profile')
    || jsonb_build_object('profiles', jsonb_build_array(sandbox_policy_json -> 'profile'))
WHERE sandbox_policy_json ? 'profile';

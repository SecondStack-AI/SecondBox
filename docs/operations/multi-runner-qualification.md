# Multi-runner qualification

`just test-multirunner` is the operator controller for the two-host release gate. It consumes the same five `SECONDBOX_RELEASE_QUALIFICATION_*` paths as the packaged harness, requires the verified KVM record to exist, and adds only `qualification/multi-runner.json` plus `qualification/multi-runner/<scenario-id>.json`. It never imports a pre-authored pass record.

## Qualified topology

Select two distinct dedicated Linux/amd64 KVM hosts declared with `deploymentMode: systemd` in the host inventory and present in the packaged KVM record. The inventory also contains any Compose host required by the KVM gate and the Actions Runner controller identity. Both selected hosts run the exact `secondbox-runner` bytes from the candidate manifest, use distinct revocable mTLS credentials, share the qualification PostgreSQL and checkpoint store, and belong to one explicit Firecracker Profile and Runner pool.

Each host has an owner-only Runner environment file and an operator-created sentinel containing the exact candidate commit. The controller requires strict known-host verification, non-interactive sudo, the expected systemd unit, and the candidate Runner digest. It does not discover hosts, credentials, services, paths, timeouts, or prior results.

## Protected environment

Configure these non-secret `release-qualification` environment variables:

- `SECONDBOX_MULTIRUNNER_QUALIFIED_DEPLOYMENT=1`, only for a fresh disposable deployment whose PostgreSQL database name contains `qualification`;
- `SECONDBOX_MULTIRUNNER_API_URL`, `SECONDBOX_MULTIRUNNER_API_CA_PEM`, `SECONDBOX_MULTIRUNNER_PROFILE`, and `SECONDBOX_MULTIRUNNER_POOL`;
- `SECONDBOX_MULTIRUNNER_RUNNER_A_ID`, `SECONDBOX_MULTIRUNNER_RUNNER_B_ID`, `SECONDBOX_MULTIRUNNER_HOST_A_ID`, and `SECONDBOX_MULTIRUNNER_HOST_B_ID`;
- `SECONDBOX_MULTIRUNNER_HOST_A_SSH`, `SECONDBOX_MULTIRUNNER_HOST_B_SSH`, and the corresponding `*_SERVICE`, `*_RUNNER_BINARY`, and `*_ENVIRONMENT_FILE` values;
- `SECONDBOX_MULTIRUNNER_HOST_SENTINEL_PATH`, `SECONDBOX_MULTIRUNNER_TIMEOUT_SECONDS`, and `SECONDBOX_MULTIRUNNER_PROBE_TIMEOUT_SECONDS`;
- `SECONDBOX_MULTIRUNNER_SSH_KNOWN_HOSTS`, containing the pinned host-key lines materialized by the workflow.

Store `SECONDBOX_MULTIRUNNER_API_TOKEN`, `SECONDBOX_MULTIRUNNER_DATABASE_URL`, and `SECONDBOX_MULTIRUNNER_SSH_PRIVATE_KEY` as protected environment secrets. The database URL contains its explicit TLS verification configuration. The workflow binds `SECONDBOX_MULTIRUNNER_CONTROLLER_HOST_ID` to the actual Actions Runner and materializes the API CA, SSH key, and known-host inputs as owner-only files.

The API authority supports only the Profile, Sandbox lifecycle, Lease, touch, and File operations used by the controller. PostgreSQL provides read-only evidence queries plus the qualification-only durable `RequestRunnerDrain` mutation.

## Destructive scenario

The controller verifies both Runners and their credentials, forces scheduler placement across them, requests a bounded drain, and proves new work avoids the draining Runner. It checkpoints a workspace marker, restores its exact bytes on the other Runner, kills a Runner process, and observes offline and uncertain state without premature replacement. Finally, it replaces one stopped Runner connection with an authenticated adversarial protocol probe, proves the old-generation result is rejected, and verifies the old Lease is fenced while exactly one current materialization remains.

Every scenario artifact is bound to the candidate manifest before the canonical generator and verifier accept the record. Missing prerequisites, unexpected state, unavailable observations, existing multi-runner output, or cleanup errors fail hard.

This controller creates Sandboxes, drains a Runner, kills and restarts services, and temporarily replaces a Runner connection. Never run it against production or shared staging. Use a fresh deployment for each attempt; a failed attempt leaves partial evidence and runtime state available for investigation instead of fabricating a clean rerun.

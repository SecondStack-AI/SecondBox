# Plan: Guided Single-Host Installation

Build a release-backed `secondbox-deploy install` wizard that takes one qualified Linux amd64 host with systemd, Docker, and KVM from read-only capability checks to a successful `secondbox run durable-coding -- ...`. The installer must keep SecondBox's deployment authority explicit: it may propose detected values, generate development authority, and materialize a reviewed single-host topology, but every accepted identity, path, capacity, network, retention, and pinned asset must be written into the installation plan and `secondbox.toml` rather than becoming a runtime default.

Implement [Polished Human-Facing CLI UI](2026-08-07-polished-cli-ui.md) first. This plan consumes its shared `internal/cliui` capability detection, Huh forms, phase rendering, progress, accessibility, and plain-output contracts rather than creating a second installer-specific terminal layer.

The first version is intentionally limited to one loopback-only development deployment and one same-host Firecracker Runner. It does not add production or remote-Runner installation, arm64 Runner support, automatic physical-disk partitioning, distribution-specific package installation, daemonless execution, fallback compute backends, or automatic upgrades. An existing dedicated XFS/Btrfs filesystem remains the preferred workspace location; the portable alternative is a fully allocated, size-bounded Btrfs filesystem image mounted persistently by systemd. Ordinary uninstall must preserve all durable data.

## Validation Commands

- `go test ./internal/install ./internal/deployconfig ./pkg/releasecontract ./pkg/releaseverify ./cmd/secondbox-deploy -count=1`
- `just verify-generated`
- `just lint`
- `just test`
- `just test-contract`
- `just test-deployment`
- `just test-compose`
- `just test-image-policy`
- `just test-release-stage`
- `just test-release-workflow`
- `just test-installer`
- `just test-installer-vm`
- `just test-non-kvm`
- `just test-firecracker`
- `just test-installer-qualified`
- `just test-scenario`

### Task 1: Define the installer operation and CLI contracts

Create a dedicated installer domain instead of adding ad hoc state to deployment-manifest types. The types in this task are the durable authority used by every later stage: strict host facts describe what was observed, an accepted plan describes every authorized action and deployment decision, and a receipt records only the stages and resources actually completed.

- [x] Add an `internal/install` package with strict, versioned `HostFacts`, `InstallPlan`, `InstallReceipt`, `CreatedResource`, stage, storage-choice, failure-class, and operation-status types.
- [x] Define canonical JSON encoding and SHA-256 plan identity; reject unknown fields, unsupported schemas, duplicate operation IDs, malformed digests, invalid enum values, and inconsistent plan/receipt identities.
- [x] Keep secrets out of plans and receipts. Store only secret categories and the exact create-only secret-file targets that the later materialization stage owns.
- [x] Model a monotonic stage sequence covering preflight, plan acceptance, host apply, release verification, asset materialization, deployment materialization, runner enrollment, Compose startup, CLI login, readiness, and smoke execution.
- [x] Validate all plan paths as absolute, normalized, non-root targets and distinguish user-owned deployment paths, installer-created host paths, an existing workspace mount, and an installer-created filesystem image.
- [x] Add process-level operation locking so two install, resume, uninstall, or purge processes cannot act on the same deployment concurrently.
- [x] Extend `cmd/secondbox-deploy` with stable grammar for `install`, `install --check`, `install --resume DIRECTORY`, the private privileged host-apply entry point, `uninstall DIRECTORY`, and `uninstall --purge DIRECTORY`.
- [x] Keep the privileged entry point absent from normal help output while giving it a greppable, domain-specific error prefix and strict argument count.
- [x] Add unit and command tests for strict decoding, canonical digests, safe paths, legal stage transitions, operation locks, usage text, exit behavior, and the absence of secret-bearing fields.

### Task 2: Implement comprehensive read-only host preflight

Make the first visible installer phase a complete capability report that performs no elevation, image pull, directory creation, mount, service change, or other persistent mutation. It must aggregate all findings in one pass and separate permanent blockers from choices the wizard or privileged stage can resolve.

- [x] Introduce injectable filesystem, process, network, clock, and user/account probes so host discovery is exhaustively unit-testable without KVM, systemd, or Docker.
- [x] Require Linux amd64 for the first qualified Runner/guest matrix and report other host architectures as unsupported rather than falling through to a later image error.
- [x] Inspect systemd as the active service manager, Docker Engine connectivity, Compose v2 availability, cgroup v2 controllers, `/dev/kvm`, `/dev/net/tun`, Btrfs kernel support, host CPU virtualization, memory, disk, and required host utilities.
- [x] Inspect Docker and host routes, interfaces, listening sockets, existing Compose projects, filesystem devices, mounts, and assigned user IDs without creating a network or container.
- [x] Check release/GHCR DNS and HTTPS reachability with bounded timeouts while distinguishing offline/transient failures from incompatible host capabilities.
- [x] Discover existing dedicated non-root XFS/Btrfs mounts, their available capacity, and candidate unassigned jailer UID ranges; do not claim reflink readiness until the later privileged mutation-isolation probe proves it.
- [x] Classify findings as pass, warning, remediable during install, needs user action, or blocked. Emit every finding in a stable text report and an internal typed result used by the wizard.
- [x] Record the exact observed facts, timestamps, device identities, and relevant software versions in the candidate plan, excluding environment variables, credentials, and unrelated host inventory.
- [x] Make ordinary `secondbox-deploy install` run this phase before presenting prompts; make `install --check` stop after the report with a useful exit status.
- [x] Add table-driven tests for missing or permission-denied KVM/TUN, cgroup v1, inactive systemd, missing Compose, Docker permission failures, unsupported architecture, port/network conflicts, low resources, candidate filesystems, and multiple simultaneous findings.

### Task 3: Build the guided planner and explicit review UX

Turn accepted host facts into a short Huh-backed wizard through the shared `internal/cliui` form and presentation interfaces. The normal path should ask only for workspace storage, resource budget, and final confirmation; advanced mode exposes every proposed value before acceptance, and accessible/plain behavior comes from the prerequisite UI contract.

- [x] Build installer-specific form groups and validation on `internal/cliui` without importing Charm packages into `internal/install`; preserve explicit input/output, EOF, cancellation, invalid-answer, accessible, and non-TTY behavior.
- [x] Offer each detected dedicated XFS/Btrfs mount plus a clearly labeled Btrfs filesystem-image choice; never offer a physical block device as a formatting target.
- [x] For the filesystem-image choice, propose a fully allocated capacity from available disk while enforcing enough room for the selected Profile workspace, the approximately 11 GB execution bundle, runner state, and a reserve for the backing filesystem.
- [x] Derive a conservative Runner/RunnerPool capacity proposal from host CPU, memory, and workspace capacity, including per-Sandbox limits, concurrent starts and operations, storage-pressure thresholds, and all nine subject quotas.
- [x] Propose loopback API, Runner control, data-plane ports, an unused guest bridge CIDR, TAP prefix, cgroup parent, unassigned jailer UID range, and non-loopback DNS upstream only after collision checks.
- [x] Materialize logical gateway mappings for both release-owned standard bundles as explicit reviewed Runner configuration; do not infer gateway authority inside the Runner or control plane.
- [x] Propose exact deployment, secret, identity, artifact, state, workspace, filesystem-image, mount-unit, CLI, and receipt paths and show which ones require sudo.
- [x] Present release version, digest-pinned images, signing-key fingerprint, expected downloads, disk allocation, generated-authority categories, retention, capacity, network choices, persistent services, and uninstall behavior in one final plan.
- [x] Require an explicit final confirmation, then atomically create a mode-`0600` canonical plan and initial receipt without generating secrets or performing host mutations.
- [x] Add installer form/model tests for the normal path, existing-filesystem path, advanced review, collision replacement, rejected proposal, EOF, accessible and non-TTY invocation, and secret-redacted output; reuse the prerequisite plan's PTY and renderer golden harness.

### Task 4: Add the narrow privileged host-apply boundary

Re-execute the same release binary through sudo only after plan confirmation, but do not give the general installer unrestricted root behavior. The root entry point must independently constrain the accepted plan to the finite set of host changes needed for SecondBox and record exactly what it created.

- [x] Invoke sudo only for the private host-apply entry point and show the exact privileged action list before sudo can prompt.
- [x] Open the plan without following symlinks, require safe ownership and mode relative to `SUDO_UID`, verify the caller-supplied plan digest, decode it strictly once, and refuse plan or receipt changes after acceptance.
- [x] Re-run root-required KVM, TUN, cgroup, UID, mount, free-space, kernel-filesystem, and path checks immediately before mutation so stale preflight evidence fails closed.
- [x] Constrain privileged operations to create-only deployment children, the accepted filesystem image and mountpoint, the exact generated systemd mount unit, daemon reload/enable/start, and an optional accepted system-wide binary target.
- [x] Reject physical block devices, `/`, home-directory roots, unresolved variables, globs, symlink components, mount-over-existing-data targets, existing unit collisions, and paths outside the operation's declared resource set.
- [x] Fully allocate the Btrfs backing file so workspace capacity cannot silently overcommit the host filesystem, then format the regular file with the release-pinned installer-tools container rather than a distribution package manager.
- [x] Generate a hardened systemd mount unit using the exact backing file and mountpoint, validate it before installation, enable it, mount it, and prove the resulting device is dedicated and non-root.
- [x] For either storage choice, perform the real FICLONE and mutation-isolation probe in the final workspace directory and fail before deployment materialization if reflinks do not work.
- [x] Create state, artifact, identity-parent, log, jail, run, network, and snapshot-template-cache directories with the exact owners and modes required by the same-host Runner; do not create host accounts for the jailer UID range.
- [x] Atomically append each created resource to the receipt. On a pre-deployment failure, remove only empty resources created by this operation and leave every pre-existing resource untouched.
- [x] Add unit tests around a fake privileged executor and Linux integration tests for path confinement, symlink races, stale plans, device rejection, allocation failure, unit collision, create-only behavior, receipt accuracy, and repeated apply.

### Task 5: Materialize a real release-backed single-host deployment

Replace the current manual substitution of synthetic development assets with a dedicated initializer that consumes a verified public release. The result remains `mode = "development"` and loopback-only, but every control-plane, Runner, standard-resource, and guest asset must come from the real release contract.

- [x] Extend the release contract and staging pipeline with a digest-pinned, minimal amd64 installer-tools OCI artifact containing only the filesystem tooling needed by host apply; do not add those tools or alternate entry points to the privileged Runner image.
- [x] Update strict release validation, fixtures, schemas, staging allowlists, checksums, image labels, publication, and source-free verification for the installer-tools artifact.
- [x] Add a release-backed single-host initializer in `internal/deployconfig` rather than changing the meaning of `init --mode development` or adding runtime defaults.
- [x] Fetch the canonical artifact-manifest URL over HTTPS, verify its strict schema, qualification evidence, standard bundles, binary digests, immutable OCI references, platform matrix, and all referenced object digests.
- [x] Pull the control-plane, Runner, microVM-artifact, and installer-tools images only by the manifest's digest-pinned references and record those exact references in the plan and deployment manifest.
- [x] Extract the microVM artifact image into a create-only temporary host directory, enforce the fixed file allowlist, and atomically publish it only after checksum, signed-manifest, component-digest, architecture, guest-protocol, and rootfs-contract verification succeeds.
- [x] Accept the artifact image's `signing.pub` only when its canonical DER fingerprint equals the separately fetched release-manifest identity; copy the verified key to its explicit host path and pin that path and fingerprint in the Runner declaration.
- [x] Generate the signed-asset catalog from the verified signed component manifest and generate standard-resource selections and RunnerPool inventory from the verified release-owned documents.
- [x] Generate unique application, platform, Runner-enrollment, and Runner-PKI authority into protected referenced files and write one complete explicit `[[runners]]` declaration without passing through the inert runner template.
- [x] Populate every Runner path, Firecracker/jailer value, capacity, network, gateway, DNS, data-plane, storage-pressure, and cold-boot setting from the accepted plan; leave snapshot-resume capability absent unless a separately verified template is actually installed.
- [x] Add tests proving there are no synthetic digests, mutable image tags, hidden Profile defaults, copied host paths, unverified trust anchors, alternate artifact sources, or partially visible artifact directories.

### Task 6: Orchestrate enrollment, startup, login, and the first Sandbox

Compose the existing deployment primitives into the promised end-to-end outcome without adding a second deployment path. Every successful stage must be observable in the receipt, and the installer must stop at the first failed prerequisite instead of catching and swallowing errors.

- [x] Refactor reusable Compose rendering/execution from `cmd/secondbox-deploy` behind a narrow internal interface while preserving environment scrubbing, exact project naming, and the existing `config`, `prepare`, `up`, and `down` commands.
- [x] Install the verified `secondbox` and `secondbox-deploy` binaries at the user-accepted location, refusing replacements except for an exact same-version, same-digest file owned by the same installation.
- [x] Issue the declared Runner identity before full same-host manifest validation, then validate the exact complete manifest and generated environment before starting any service.
- [x] Prepare bundled PostgreSQL and object storage, start the control plane and same-host Runner with the reviewed Compose overlays, and apply standard RunnerPool/Profile resources through the existing idempotent resource engine.
- [x] Wait with bounded stage-specific deadlines for control-plane health/readiness, resource application, authenticated Runner registration, Runner readiness, and advertised cold-boot capacity.
- [x] Log in the CLI with the generated loopback platform authority for the invoking user without printing or copying the token into the plan, receipt, process arguments, or logs.
- [x] Execute `secondbox run durable-coding -- python3 -c 'print("hello from a microVM")'`, require the expected output and zero exit status, and record only the smoke result and public Sandbox identifiers in the receipt.
- [x] Print a final receipt summary with deployment manifest, durable workspace, generated authority, logs, installed versions, health commands, resume command, uninstall command, and the exact next `secondbox run` example.
- [x] On readiness or smoke failure, preserve the deployment and stage evidence for diagnosis and return a nonzero status; do not report a partially ready install as success.
- [x] Add command-level tests with fake Docker, HTTP, resource-apply, Runner-readiness, CLI-login, and smoke executors, including timeouts and failures at every orchestration boundary.

### Task 7: Implement safe resume, diagnostics, uninstall, and purge

Make interruption and service removal predictable without weakening workspace durability. Resume must continue from verified evidence, uninstall must preserve data by default, and purge must never turn receipt data into unchecked deletion authority.

- [x] Have `install --resume DIRECTORY` lock the operation, reload the strict plan and receipt, revalidate their digest and host identities, re-probe affected prerequisites, and continue from the first incomplete stage.
- [x] Verify completed-stage postconditions before skipping them: exact files and modes, image digests, artifact hashes, mount/device identity, manifest digest, Runner identity, Compose project, CLI binary, and service health.
- [x] Refuse conflicting replay when a completed resource changed, disappeared, moved devices, or belongs to another deployment; report the exact recovery action instead of reconstructing empty state.
- [x] Classify and render blocked, needs-action, retryable, and internal failures with stage-specific remedies and stable exit statuses.
- [x] Extend the bounded support bundle to include the redacted preflight report, plan digest, receipt, non-secret manifest inspection, Docker/Compose status, systemd mount status, Runner health/log tail, filesystem facts, and control-plane health without token, key, certificate-private-key, or workspace contents.
- [x] Implement ordinary uninstall as Compose shutdown plus removal of only non-data runtime resources created by the operation; preserve the manifest, secrets, identities, database/object-store volumes, artifact bundle, Btrfs image or existing workspace directory, mount unit, and receipt.
- [x] Make uninstall output the exact preserved paths and the commands needed to resume the deployment or enter the separate purge workflow.
- [x] Before purge, require a healthy inspection or explicit acknowledgement that state cannot be inspected, list every exact target and its ownership evidence, refuse unresolved/symlink/broad targets, and require explicit typed confirmation.
- [x] Purge only resources whose plan-and-receipt identity still matches. Never use recursive deletion against `/`, a workspace root supplied without installation ownership, a physical mount, a missing variable, or a glob.
- [x] Add failure-injection tests across every stage, repeated resume tests, changed-resource conflicts, concurrent-operation locking, uninstall preservation, purge refusals, support-bundle redaction, and exact created-resource deletion.

### Task 8: Ship the verified bootstrap and repair installation documentation

Make installation discoverable without turning the bootstrap shell into the installer. The release binary remains the implementation and verification boundary; the script only selects, verifies, and executes that binary.

- [x] Generate a small POSIX `install.sh` release asset with the exact release version and Linux amd64 `secondbox-deploy` digest embedded from the staged artifact manifest.
- [x] Have the bootstrap require Linux amd64, HTTPS download the one canonical binary to a temporary directory, verify it with `sha256sum` or `shasum`, and `exec secondbox-deploy install`; it must never invoke sudo, modify shell profiles, or implement host setup itself.
- [x] Provide an equivalent multi-command download-and-inspect path for users unwilling to pipe a script into a shell.
- [x] Include the bootstrap in the release allowlist and `SHA256SUMS`, test its generated content, and publish it at GitHub's stable `releases/latest/download/install.sh` coordinate. Treat a future `secondbox.dev/install.sh` redirect as external distribution work, not a runtime dependency.
- [x] Update release staging and source-free tests so the bootstrap cannot name a binary/version/digest that differs from the artifact manifest.
- [x] Rewrite the README getting-started section around the guided single-host install and remove the stale hard-coded v0.1.4 instructions from the current v0.3.1 tree.
- [x] Keep the manual release-binary, production deployment, remote Runner, and source-checkout development paths clearly separated from the guided quickstart.
- [x] Document preflight classifications, every wizard choice, disk/download expectations, Btrfs-image durability tradeoffs, sudo actions, files and services created, resume, diagnostics, uninstall, purge, and the lack of automatic updates.
- [x] Update deployment, microVM distribution, release distribution, backup/restore, and scenario-qualification documentation with the new single-host path and its explicit authority/storage boundaries.
- [x] Add documentation contract tests that keep the public command, release asset name, generated help, and bootstrap coordinate synchronized.

### Task 9: Qualify capability-based installation across systemd hosts

Prove the installer at the boundaries its unit tests cannot simulate. Distribution identity must remain informational: admission comes from capability evidence, while a representative matrix catches assumptions about systemd, Docker, mount tooling, and host layout.

- [x] Add `just test-installer` for all non-privileged installer, release, transcript, fake-executor, failure-injection, and Compose contract tests runnable on ordinary CI hosts.
- [x] Add a controller-driven `just test-installer-vm` harness for disposable current Debian/Ubuntu, Fedora-family, and rolling-release systemd VMs with Docker and nested KVM where available.
- [ ] In each VM, start from no SecondBox files, run the staged public bootstrap, prove the read-only phase changed nothing, accept the Btrfs-image path, complete installation, reboot, reconnect, and verify the mount, Compose services, Runner, CLI login, and hello-world Sandbox recover.
- [ ] Exercise the existing-XFS/Btrfs path separately, including real reflink/mutation isolation and refusal of root-filesystem, ext4, symlink, and same-device candidates.
- [ ] Interrupt each durable stage in disposable VMs and prove resume neither repeats unsafe mutations nor downloads/re-extracts an already verified 11 GB bundle.
- [ ] Prove ordinary uninstall preserves a retained Sandbox workspace and all recovery authority, then resume the deployment and execute inside the same Sandbox generation lineage.
- [ ] Prove purge removes only the listed installation-owned resources and cannot delete an injected neighboring directory, unrelated Compose project, foreign mount, or altered receipt target.
- [x] Add `just test-installer-qualified` for a real KVM host using published-style digest-pinned assets; emit bounded JSON evidence with source commit, release manifest digest, host facts, filesystem identity, reboot result, pass count, and wall clock.
- [x] Require fresh clean-checkout installer qualification evidence during release staging alongside the existing `just test-scenario` evidence, and bind its digest into the release artifact manifest without weakening the existing qualified-host gate.

Validation note (2026-08-08): after the review hardening pass, `just verify-generated`, `just lint`, `just test`, `just test-non-kvm`, the ordinary installer suite, unattended preflight smoke, release staging, contract, deployment, Compose, SDK, image-policy, and CLI UI gates passed. The controller-driven VM suite was discoverable but skipped because `SECONDBOX_INSTALLER_VM_CONTROLLER` and `SECONDBOX_INSTALLER_VM_IMAGES_JSON` were not configured. The non-skipping Firecracker gate reported the missing `SECONDBOX_RUNNER_FIRECRACKER_PATH`; installer qualification reported that `SECONDBOX_REQUIRE_QUALIFIED_INSTALLER=1` and its published-style inputs were absent; scenario qualification reported that its required KVM/TUN, signed microVM artifact, trust anchor, and reflink workspace inputs were absent. Those qualified-host checkboxes remain open below.
- [ ] Run `just verify-generated`, `just lint`, `just test`, all contract/Compose/release/installer suites, `just test-firecracker`, `just test-installer-qualified`, and `just test-scenario` before handoff; record any host-only commands and resulting evidence paths in the implementation handoff.

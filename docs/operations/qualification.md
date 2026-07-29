# Qualification gates

SecondBox separates portable, non-KVM validation from tests that mutate a dedicated Linux runner host. The root `Justfile` is the authoritative command index. Ordinary CI never treats a skipped Firecracker test as qualification evidence.

## Non-KVM candidate gate

The ordinary CI workflow runs `scripts/test-clean-clone-isolation.sh --non-kvm` for every candidate commit. The gate refuses a dirty source tree, clones the exact commit without local object sharing, isolates the Go module, Go build, and npm caches, rejects local Go replacements, escaping symbolic links, and executable source references to SecondStack, then runs the complete portable matrix inside that clone:

- `npm ci --ignore-scripts && just verify-generated` installs the pinned TypeScript validator and verifies that the Go, TypeScript, and Python clients exactly match `contracts/openapi/v1/secondbox.openapi.json`.
- `just test` runs control-plane Go tests, the PostgreSQL store conformance suite, control-plane vetting, Python client syntax validation, runner Go tests, and runner vetting. `SECONDBOX_TEST_DATABASE_URL` must name a disposable database containing `test` or `conformance`; the suite resets its `sandbox` schema.
- `just test-contract` runs the public HTTP, control-plane-to-runner baseline, guest control, and generated Go client contract tests without KVM.
- `just test-compose` starts PostgreSQL and the built control plane with `scripts/compose-test.yml`, applies the baseline migration and test-only runner-pool compatibility fixture, requires database-backed readiness through a loopback-only API port, runs the live Go and TypeScript SDK contract tests, and tears down the project and volume.
- `just test-image-policy` checks the guest browser-surface and trust-anchor policies without building or booting a guest image.
- `just build-artifacts` builds the `secondbox`, `secondboxd`, `secondbox-runner-identity`, `secondbox-runner`, `secondbox-guest-agent`, and `secondbox-artifact-evidence` binaries, then writes `dist/SHA256SUMS`.
- `scripts/test-clean-clone-isolation.sh --non-kvm` verifies that the matrix leaves tracked and unignored source unchanged and that the tested commit is exactly the source commit.
- `go test ./tests/release ./tests/operations` enforces the compatibility and non-publication policies.

`scripts/test-non-kvm.sh` is the authoritative matrix orchestrator used inside the clone. Passing this matrix establishes only portable source, contract, PostgreSQL, Compose, and binary-build evidence for the exact commit. It does not establish KVM, multi-runner, durability, security-boundary, upgrade, compatibility-skew, signature, or publication qualification.

The optional Compose `same-host-runner` profile is deployment wiring, not part of this portable gate and not KVM evidence. The backup script enforces a PostgreSQL-wide publication fence and rejects non-quiescent runtime authority. The checkpoint receiver retains its verified spool and leaves PostgreSQL publication authority unchanged when object-store publication is interrupted, then completes idempotently after the store and receiver recover. `just test-backup-restore` restores a recovery point into isolated database and object namespaces, enrolls a fresh mTLS Runner protocol authority, streams and hashes the retained checkpoint through the production scheduler and checkpoint sender, and proves current and stale generation behavior through the restored HTTP API. This qualifies portable recovery mechanics, not a privileged Firecracker boot.

## Packaged qualification harness

`scripts/run-packaged-release-qualification.mjs` is the operator-driven runner for the KVM, durability, data-plane, network, and security gates. It accepts only explicit positional inputs: the reconstructed candidate directory, that directory's canonical `release-subjects.json`, a qualified-host inventory, a scenario-controller directory, an empty output directory, and one or more named gates. It does not build from the checkout, choose hosts, discover a prior result, synthesize a pass, or provide a prerequisite default.

The host inventory conforms to `release/qualification-hosts-schema.json`. A host declaration is inventory, not proof: the `qualified-host-prerequisites` controller must inspect the actual host before any KVM scenario and capture `/dev/kvm`, TUN/TAP, cgroup-v2 controller, jailer-filesystem, network-namespace, nftables, storage, memory, disk, and privilege evidence. KVM qualification requires two dedicated Linux/amd64 KVM Runner hosts and includes both packaged systemd and digest-pinned Compose deployments. Durability also requires two such hosts; data-plane, network, and security require at least one.

The controller directory has an executable regular file at `<gate>/<scenario-id>` for every scenario in `release/qualification-requirements.json`, with no missing or extra file inside a requested gate. The harness invokes each executable directly, without a shell command string, with these six arguments:

1. canonical candidate directory;
2. canonical subject manifest;
3. canonical qualified-host inventory;
4. a new per-scenario artifact directory;
5. gate;
6. scenario ID.

The controller must drive the installed package, digest-pinned images, signed guest bundle, and released clients on the named hosts. Source-tree Go tests, a copied success marker, and a controller that merely describes an intended scenario are not release qualification. The controller writes `result.json` conforming to `release/qualification-scenario-result-schema.json` plus every referenced raw observation into its artifact directory. The result binds the exact release version, commit, subject-manifest digest, host IDs, and subject IDs. The harness captures controller stdout and stderr, rejects unknown hosts or subjects, missing/extra/symbolic-link artifacts, checksum drift, skipped/failed controllers, and candidate byte drift, then creates the gate record and runs the canonical record verifier.

`just test-firecracker` is the packaged KVM-only entry point and requires all five paths explicitly:

```sh
export SECONDBOX_RELEASE_QUALIFICATION_CANDIDATE_DIRECTORY=/secure/candidate
export SECONDBOX_RELEASE_QUALIFICATION_SUBJECT_MANIFEST=/secure/candidate/release-subjects.json
export SECONDBOX_RELEASE_QUALIFICATION_HOSTS_FILE=/secure/qualification/qualified-hosts.json
export SECONDBOX_RELEASE_QUALIFICATION_CONTROLLER_DIRECTORY=/secure/qualification/controllers
export SECONDBOX_RELEASE_QUALIFICATION_OUTPUT_DIRECTORY=/secure/qualification/output
just test-firecracker
```

The protected workflow calls the same harness once for all five non-multi-runner gates after reconstructing and hashing the protected candidate. A missing packaged artifact, controller, host, KVM capability, destructive-test prerequisite, result receipt, or evidence file fails the workflow; it is never represented as a skip.

## Runner qualification coverage matrix

The following matrix maps the extracted host tests to current SecondBox behavior. “Covered” means the named automated test exercises an implementation that still exists. “Partial” names the narrower claim the repository can prove and the unimplemented or externally controlled remainder.

| Area | Current evidence | Status and claim |
| --- | --- | --- |
| Firecracker and jailer | `TestSmokeBootFirecracker`, `TestSmokeJailedTapGeneratedImage`, launch/config/version/admission tests, `microvm-stage-check.sh` | Covered for a qualified Linux/KVM host, exact Firecracker version, jailed launch, TAP attachment, cgroup/resource configuration, and startup failure cleanup. |
| Guest agent and host↔guest protocol | `runner/internal/guest`, guest protocol conformance, `TestSmokeGeneratedImageBootsControlAndRuntime`, `TestSmokeGeneratedToolExecutorImageReadiness` | Covered for negotiated identity/generation/features, bounded Exec/File operations, cancellation, output credit, descriptor-pinned paths, and runtime-secret placement. |
| Image pipeline and immutable assets | `just test-image-policy`, `tests/image-pipeline`, artifact verifier tests, generated-image smoke tests | Covered for input policy, browser surface, trust anchor, signed manifest and component binding, checksums, provenance shape, immutable runtime/toolchain selection, and boot of a generated bundle. |
| Host and per-assignment network enforcement | `runner/scripts/microvm_host_network_test.go`, `microvm-network-namespace-test.sh`, `runner/internal/networkpolicy`, `runner/internal/firecracker/network_policy_test.go`, `dns_proxy_test.go` | Covered for idempotent host bridge/firewall setup, per-TAP default deny, exact CIDR/domain rules, controlled DNS, protected destinations, isolated pin state, and enforcement-failure propagation. |
| Metadata SSRF and private ranges | `TestIPPolicyRejectsMetadataSSRFAndPrivateRanges`, `TestDNSResolutionRejectsMetadataSSRFAndRebinding`, `TestNFTablesNetworkPolicyEnforcerRejectsProtectedDNSAnswerBeforeMutation` | Covered for direct and DNS-derived IPv4/IPv6 metadata, loopback, link-local, private, carrier-grade NAT, multicast, Runner, and management destinations. |
| DNS rebinding and pinning | `TestDNSResolutionPinsPublicAnswersPerSandbox`, `TestDNSResolutionRejectsMetadataSSRFAndRebinding`, `TestDNSPinsAreBoundedAndTTLIsExplicit`, `TestNFTablesNetworkPolicyPinsAreSandboxScopedAndExpire`, DNS response validation tests | Covered for exact-query validation, bounded CNAME ownership, unrelated-answer rejection, per-address-family answer stability, TTL/capacity bounds, per-assignment firewall pins, expiry, and cross-Sandbox isolation. |
| Runner process restart | The restart section of `TestSmokeGeneratedToolExecutorImageReadiness` | Covered only for stopping and recreating the Manager process and remounting an existing workspace without cross-compartment disclosure. This is not host reboot qualification. |
| Disk pressure | `TestThreatModelJailedGuestEscapeAndResourceExhaustion`; storage-pressure, restore-spool, and released-workspace cleanup tests; ext4 and dm-thin pressure-probe tests | Covered portably for guest workspace-size exhaustion, explicit threshold ordering, warning/admission/recovery transitions, atomic workspace and restore-spool reservations, failed/consumed/expired restore cleanup, typed probe and cleanup failures, generation-fenced cleanup that preserves active and replacement workspaces, rejection of host-root storage, and exact-pool dm-thin sampling without fallback. Destructive real-host fill and cleanup recovery still require the controller described below. |
| Cleanup and bounded teardown | Manager admission, startup-orphan, natural-exit, shutdown, TAP/IP retention, nftables removal, workspace, and thin-device tests; Firecracker smoke cleanup | Covered for current direct-Runner ownership. Cleanup failure remains fail-closed and prevents premature guest identity reuse. |
| No-session port isolation | `TestNFTablesNetworkPolicyKeepsUnsolicitedInboundClosedWithoutPortSessions`, `microvm-network-namespace-test.sh` | Covered only for the current default-deny posture: no DNAT/redirect or port-specific accept rule is created, unsolicited traffic toward a TAP is dropped, and guests cannot reach Runner host listeners. |
| Authenticated published-port sessions | `TestPostgresPortSessionAuthorityPolicyTokenAndAccounting`, `TestPublicPortTunnelIsBinarySingleUseBackpressuredAndAccounted`, `TestRunnerPortProxyIsFencedBackpressuredAndCancelled`, `TestRunnerPortProxyRejectsStaleFenceAndClosesOnRunnerDisconnect`, `TestGuestPortProxyUsesOnlyApprovedLoopbackDialAndByteCredit`, `TestNegotiateGuestProtocolOverFirecrackerVsockTransport` | Covered for profile-approved TCP/HTTP ports, Project/generation/Lease fencing, one-time expiring tunnel credentials, binary proxying, bounded bidirectional credit, loopback-only guest dialing, disconnect cleanup, and useful-activity accounting. The Runner creates no public listener or raw host-port publication. |

The removed `launcher_probe`, `privileged_launcher`, `sandbox_host_http`, `source_binding_client`, and harness-source digest tests belonged to the deleted SecondStack local launcher, Agent harness, egress binding, or private host HTTP APIs. They are intentionally not ported. The old global launcher network-posture tests are replaced by the current host-network script tests plus per-assignment nftables and DNS tests; retaining both would test an implementation that no longer exists.

## Reboot and destructive disk-pressure qualification

Reboot recovery and destructive real-host disk pressure require operator controllers because the process running on the mutated host cannot survive or safely coordinate every event it validates. The repository's portable disk-pressure tests establish the Runner policy and probe contract, but they are not accepted by the packaged harness as destructive host evidence.

A reboot controller must persist the expected assignment, workspace, TAP, cgroup, and process evidence outside the runner; reboot the host; restart the packaged runner through its system service; and verify bounded orphan cleanup, generation fencing, workspace integrity, and readiness before scheduling new work.

A disk-pressure controller must use a dedicated bounded filesystem or thin pool; drive the packaged Runner through warning, admission-denied, and recovery thresholds without filling the host root filesystem; capture the bounded pressure evidence and typed assignment rejection; release retained data; and prove readiness and capacity recover without corrupting an existing workspace.

## Multi-runner qualification

`just test-multirunner` drives two explicit remote systemd Runner hosts after the packaged KVM record exists. Both hosts must be distinct dedicated Linux/amd64 KVM machines in the qualified inventory, run the candidate's exact Runner binary, use distinct revocable mTLS identities, and share the qualification PostgreSQL and checkpoint authorities. The controller proves placement, bounded drain, stopped-Sandbox relocation with exact checkpoint bytes, crash uncertainty without premature replacement, authenticated stale-result rejection, and cross-Runner generation fencing. It adds only the multi-runner record and candidate-bound scenario artifacts to the existing qualification output.

The controller is destructive and refuses implicit hosts, credentials, paths, timeouts, SSH trust, API trust, or database targets. Run it only against a fresh disposable qualification deployment whose database name contains `qualification`; partial state is intentionally retained after failure. The complete protected-environment inventory, host preparation, secret boundary, and scenario sequence are documented in [multi-runner qualification](multi-runner-qualification.md).

## Structured release records

Privileged release qualification is accepted only through the six records named `qualification/kvm.json`, `qualification/multi-runner.json`, `qualification/durability.json`, `qualification/data-plane.json`, `qualification/network.json`, and `qualification/security.json`. A log line or success marker is not qualification evidence.

Each record must conform to `release/qualification-record-schema.json` and pass `scripts/verify-release-qualification-record.mjs` against the exact candidate `release-subjects.json`. The record hashes the manifest and repeats its complete subject set. Every subject ID, kind, locator, and SHA-256 must match; an omitted, extra, duplicate, blocked, or changed subject rejects the record. Every required scenario must appear exactly once with passing status and at least one regular, non-symbolic-link, checksum-matching artifact inside the evidence directory.

`release/qualification-requirements.json` is the authoritative scenario matrix. KVM qualification requires two dedicated Linux/amd64 KVM Runner hosts and proves both packaged systemd and Compose deployment. Multi-runner and durability qualification require two dedicated KVM Runner hosts. Data-plane, network, and security records require at least one such host. The verifier also rejects duplicate host identities, an incomplete scenario set, unexpected scenarios, candidate identity drift, a changed subject-manifest digest, and a completion timestamp before the start timestamp.

The non-multi-runner records are created only by `scripts/run-packaged-release-qualification.mjs` from controllers that it executes during the protected job. The workflow does not import pre-authored KVM, durability, data-plane, network, or security records from a host directory. Per-scenario result receipts and captured observations remain beneath `qualification/scenarios/<gate>/<scenario-id>/` and every file is checksum-bound into its record.

The data-plane record covers buffered and streaming Exec, typed spawn failures, deadline/cancellation/output limits, PTY detach and reattach, file transfer, Artifacts, exposed ports, the Go and TypeScript SDKs, the CLI, and the Flue adapter. The network record covers default deny for private, loopback, link-local, cloud-metadata, Runner-host, DNS-rebinding, and unobserved-domain destinations, plus explicit-profile allow and the authenticated tunnel as the exclusive exposed-port path. The security record covers application tenant isolation, the malicious-guest host boundary, Runner credential revocation, artifact substitution rejection, and control-plane secret isolation.

`.github/workflows/release-qualification.yml` is the only Actions authority allowed to upload `release-qualification-<commit>`. It is manually dispatched from protected `main`, runs in the protected `release-qualification` environment on the `secondbox-release` Runner group, and requires the `secondbox-release-qualification` host label. It reconstructs the exact candidate identified by a canonical protected candidate run, verifies all six records, and records the repository, workflow, event, environment, commit, run, attempt, job, Runner name, OS, and architecture. The artifact also includes the environment API response captured by the job; the verifier requires a reviewer, self-review prevention, and a protected-branch-only policy.

The canonical workflow also requires its actual Actions Runner name to appear as a host ID in every record. The evidence workflow queries the Actions run and jobs APIs for the supplied run ID and compares them with that protected workflow identity. It rejects another workflow path, branch, event, commit, failed or duplicate job, generic Runner group, missing label, changed Runner name, changed run attempt, absent host binding, or caller-authored run ID. Merely uploading a same-repository artifact with the expected name is not authority. The qualified host must exercise already packaged and content-addressed release subjects; source-tree binaries or replacement subject manifests are not release qualification.

## Release qualification status

No current commit is publication-eligible merely by passing ordinary CI. The release evidence gate remains blocked until all of these produce artifact-backed passing records for the same candidate commit:

- packaged KVM and jailer execution on a dedicated host;
- two-runner placement, drain, loss, stale-message fencing, and stopped-Sandbox relocation;
- integrated immutable checkpoint and Artifact publication, cross-runner restore, retention, garbage collection, restart, and restore-drill evidence;
- buffered and streaming data-plane operations, typed failures, PTY, file, Artifact, port, SDK, CLI, and adapter behavior;
- complete default-deny network policy and authenticated exposed-port behavior;
- the complete application-tenancy, malicious-guest, host, network, credential, and supply-chain security matrix;
- released-client, Runner-generation, guest-generation, database-upgrade, rolling-control-plane, reachable-profile, checkpoint-format, and Artifact-manifest compatibility scenarios;
- Go, npm, binary, and digest-pinned container SBOM and vulnerability results;
- registry-backed minimum dependency-age evidence;
- complete license evidence, package checksums, release-key signatures, and provenance.

The workflow produces a blocked evidence bundle when a matrix is absent; `scripts/verify-release-publication-eligibility.sh` rejects it. See [release evidence](release-evidence.md) and [compatibility status](compatibility.md).

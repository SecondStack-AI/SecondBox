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

## Firecracker qualified-host contract

Run `sudo --preserve-env just test-firecracker` only on a disposable, dedicated Linux runner host. The gate fails before running tests unless every prerequisite is explicit:

- `SECONDBOX_FIRECRACKER_QUALIFIED_HOST=1` acknowledges that the host is dedicated to destructive qualification.
- `/dev/kvm` and `/dev/net/tun` are readable and writable by the root test process. Nested virtualization must be enabled when the host is itself virtualized.
- cgroup v2 is mounted and exposes the `cpu`, `memory`, and `pids` controllers. The filesystem containing the jailer root permits device nodes.
- `go`, `ip`, `iptables`, `ip6tables`, `nft`, `mkfs.ext4`, `findmnt`, `mountpoint`, `sysctl`, and `timeout` are installed. The process has the effective capabilities needed to create network namespaces, TAP devices, routes, firewall rules, mounts, and cgroups.
- `SECONDBOX_RUNNER_FIRECRACKER_PATH` and `SECONDBOX_RUNNER_FIRECRACKER_JAILER_PATH` identify the exact executable release under test.
- `SECONDBOX_RUNNER_MICROVM_ARTIFACTS_DIR` contains `kernel`, `rootfs.ext4`, `shared.img`, provenance, checksums, manifest, and signature files from one immutable bundle. `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY` and `SECONDBOX_RUNNER_ARTIFACT_PUBLIC_KEY_SHA256` identify its trusted verification key and pinned DER fingerprint.
- `SECONDBOX_RUNNER_NETWORK_POLICY_NFT_PATH`, `SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_PINS`, `SECONDBOX_RUNNER_NETWORK_POLICY_MAX_DNS_TTL`, `SECONDBOX_RUNNER_NETWORK_POLICY_RUNNER_ADDRESSES`, `SECONDBOX_RUNNER_NETWORK_POLICY_MANAGEMENT_CIDRS`, and `SECONDBOX_RUNNER_NETWORK_POLICY_DNS_UPSTREAM` identify the exact fail-closed nftables and controlled-DNS qualification boundary.
- `SECONDBOX_RUNNER_STORAGE_PRESSURE_RECOVERY_PERCENT`, `SECONDBOX_RUNNER_STORAGE_PRESSURE_WARNING_PERCENT`, and `SECONDBOX_RUNNER_STORAGE_PRESSURE_ADMISSION_DENY_PERCENT` define the ordered pressure hysteresis. Workspace and `SECONDBOX_RUNNER_CHECKPOINT_RESTORE_SPOOL_DIR` filesystems must be dedicated, non-root, and distinct; dm-thin additionally requires the exact pool device.
- The host has enough free RAM and disk for the profile under test plus bounded failure overhead. Qualification must not share its thin pool, workspace directory, run directory, bridge, TAP prefix, or cgroup subtree with a production runner.

The gate verifies the signed bundle, validates privileged staging, boots the jailed Firecracker/network path through the existing smoke suite, and runs the destructive network-namespace policy test. A non-root invocation, missing artifact, inaccessible kernel feature, or skipped host suite is a failure.

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

Reboot recovery and destructive real-host disk pressure require an external host controller because the process running the test cannot survive or safely coordinate every event it validates. The repository's portable disk-pressure tests establish the Runner policy and probe contract, but no destructive filesystem or thin-pool fill record is included in `test-firecracker`; host-level pressure qualification may not be claimed as passing.

A reboot controller must persist the expected assignment, workspace, TAP, cgroup, and process evidence outside the runner; reboot the host; restart the packaged runner through its system service; and verify bounded orphan cleanup, generation fencing, workspace integrity, and readiness before scheduling new work.

A disk-pressure controller must use a dedicated bounded filesystem or thin pool; drive the packaged Runner through warning, admission-denied, and recovery thresholds without filling the host root filesystem; capture the bounded pressure evidence and typed assignment rejection; release retained data; and prove readiness and capacity recover without corrupting an existing workspace.

## Multi-runner qualification

`just test-multirunner` deliberately fails until a remote-runner qualification controller is supplied. A qualified topology requires two independent hosts that each satisfy the Firecracker contract, distinct revocable runner credentials, one compatible runner pool, shared PostgreSQL authority, shared S3-compatible checkpoint storage, and automated placement, drain, loss, stale-message, fencing, and stopped-Sandbox relocation scenarios.

## Structured release records

Privileged release qualification is accepted only through the six records named `qualification/kvm.json`, `qualification/multi-runner.json`, `qualification/durability.json`, `qualification/data-plane.json`, `qualification/network.json`, and `qualification/security.json`. A log line or success marker is not qualification evidence.

Each record must conform to `release/qualification-record-schema.json` and pass `scripts/verify-release-qualification-record.mjs` against the exact candidate `release-subjects.json`. The record hashes the manifest and repeats its complete subject set. Every subject ID, kind, locator, and SHA-256 must match; an omitted, extra, duplicate, blocked, or changed subject rejects the record. Every required scenario must appear exactly once with passing status and at least one regular, non-symbolic-link, checksum-matching artifact inside the evidence directory.

`release/qualification-requirements.json` is the authoritative scenario matrix. KVM qualification requires two dedicated Linux/amd64 KVM Runner hosts and proves both packaged systemd and Compose deployment. Multi-runner and durability qualification require two dedicated KVM Runner hosts. Data-plane, network, and security records require at least one such host. The verifier also rejects duplicate host identities, an incomplete scenario set, unexpected scenarios, candidate identity drift, a changed subject-manifest digest, and a completion timestamp before the start timestamp.

The data-plane record covers buffered and streaming Exec, typed spawn failures, deadline/cancellation/output limits, PTY detach and reattach, file transfer, Artifacts, exposed ports, the Go and TypeScript SDKs, the CLI, and the Flue adapter. The network record covers default deny for private, loopback, link-local, cloud-metadata, Runner-host, DNS-rebinding, and unobserved-domain destinations, plus explicit-profile allow and the authenticated tunnel as the exclusive exposed-port path. The security record covers application tenant isolation, the malicious-guest host boundary, Runner credential revocation, artifact substitution rejection, and control-plane secret isolation.

An external controller may upload a same-repository Actions artifact named `release-qualification-<commit>`. The evidence workflow accepts its run ID as an optional manual input and verifies the records before import. The controller must exercise already packaged and content-addressed release subjects; source-tree binaries or controller-authored replacement subject manifests are not release qualification.

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

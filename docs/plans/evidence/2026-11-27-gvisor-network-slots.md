# gVisor network slot expansion qualification

Source: working tree based on `b5f1e6371696740c6f0c88020aeeedfc0fdeae83`,
2026-11-27. This qualifies address allocation and selected lifecycle paths;
it does not qualify 256 concurrently running sentries or a startup SLO.

The allocation keeps the original `/24` for every profile and adds three banks
at third-octet offsets 16, 32, and 48. All 16 profiles retain their original
slots `0`-`62`, DNS addresses, and interface/namespace names. The new bound is
256 slots per profile in both Runner and deployment-manifest validation.

## Mechanism and runtime evidence

Tests ran in disposable Docker network/cgroup namespaces on an amd64 Linux
Btrfs host. The tool image was
`ghcr.io/secondstack-ai/secondbox/runner-gvisor@sha256:08a9876c38b9bdab91ce72e2e5672d5070cd0f1da1f6d768a0a30db0218d1a1b`;
the test executable and guest agent were built from the changed source.
The flat root came from the local v0.33.0 release assets; `runsc` matched the
SHA-512 pin in `runner/scripts/fetch-runsc.sh` (release `20260817.0`).
Each container used its own network namespace, never the host network.

The network-only checks can be repeated with an existing persistent
`QUALIFICATION_DIR` containing a writable `tmp/` directory:

```sh
(cd runner && CGO_ENABLED=0 go test -c -tags listener_qualification \
  -o "$QUALIFICATION_DIR/gvisor.test" ./internal/gvisor)
docker run --rm --privileged --network none \
  --entrypoint /qualification/gvisor.test \
  --mount "type=bind,src=$QUALIFICATION_DIR,dst=/qualification" \
  --env TMPDIR=/qualification/tmp \
  ghcr.io/secondstack-ai/secondbox/runner-gvisor@sha256:08a9876c38b9bdab91ce72e2e5672d5070cd0f1da1f6d768a0a30db0218d1a1b \
  -test.v -test.count=1 \
  -test.run 'Test(NetworkSlotExpansionQualification|AttributedExecutionNetworkQualification)$'
```

- Unit/race checks passed for all 4,096 profile/slot pairs, bank boundaries,
  legacy mapping preservation, reserved ranges, concurrent allocation,
  exhaustion/reuse, and validation at 256/257.
- `TestNetworkSlotExpansionQualification` passed with 256 simultaneous kernel
  links in profile 15 and a legacy slot in profile 14. Host and namespace routes
  selected the expected interfaces. Reconciliation removed profile 15's links,
  namespaces, policies, and orphaned NAT-only policy, retained profile 14,
  and allowed slot 255 to be recreated.
- That test exposed asynchronous veth disappearance after namespace deletion.
  Reconciliation now tolerates the specific already-absent-device error, as
  ordinary teardown already did; other errors still fail startup.
- `TestAttributedExecutionNetworkQualification` passed at profile/slot pairs
  `0/0`, `15/64`, and `15/255`: real attributed relays, host-client denial,
  listener revocation, and startup listener-table reclamation.
- Real runsc network policy qualification passed at slots 0 and 255: DNS pin
  admission, disjoint-name/private/metadata denial, pin rotation/expiry, NAT,
  and fencing. Earlier slots were reserved in memory, without launching sentries.
- Real backend boot, conformance, supervisor-loss delivery, deny-all routing,
  attachment round-trip/crash recovery, and selected-image boot checks passed.
  The fixture needed its required image-verification directory configured.

## Qualification limits

Required `just verify-generated`, `just test` (both Go modules and vet), and
`just test-contract` passed. The gVisor package also passed `go test -race` and
`go vet -tags listener_qualification`. The full test gate used a dedicated
disposable PostgreSQL 18.4 database and a short persistent `TMPDIR`:
longer paths exceed the Unix socket limit in existing attribution tests.

The broad runtime suite failed `TestQualifiedGVisorAttributedRevocation/exec-end`
with a gateway read timeout. The same failure reproduced with the original
allocator from the base commit, using only the fixture directory correction.
The expiry and fence subtests passed. The suite is therefore not fully green.

`just test-scenario` stopped at preflight because
`SECONDBOX_SCENARIO_EXECUTION_IMAGE` and the associated registry/DNS inputs were
not configured. No complete control-plane scenario or Kubernetes qualification
was performed. The isolated network checks do not qualify shared Docker host
firewall behavior, a no-KVM host class, or production concurrency/performance;
the runtime exercised systrap on this KVM-capable workstation.

The optional repository formatting gate also fails on the unchanged
`runner/internal/runnercontrol/grpc_connector.go`; `git show HEAD:<path> | gofmt -d`
confirms the same formatting issue in the base commit. Changed Go files are formatted.

Local raw logs are retained in `.tmp/gvisor-network-slots/` (git-ignored).
Temporary build assets, database, and test containers are removed at handoff.

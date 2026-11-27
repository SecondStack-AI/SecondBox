# gVisor network slot expansion qualification

Source: working tree based on `2310b4c6f20ff4ffb9802f34772880c3f5a624dd`,
2026-11-27. This qualifies 1,024 address slots per profile and selected lifecycle
paths. It does not qualify 1,000 concurrently running sentries or a startup SLO.

## Address layout and coexistence

All 16 profiles retain slots `0`-`255` from the earlier PR implementation,
including slots `0`-`62` from released Runners. DNS and interface/namespace names
also stay unchanged. A profile has sixteen banks of 64 independent `/30` links:

- Banks `0`-`3` use `169.254.(104+profile+16*bank).0/24`.
- Banks `4`-`15` use `198.18.(profile+16*(bank-4)).0/24`.
- Host/guest final octets are `4*(slot%64)+1` and `4*(slot%64)+2`.

The exact combined ranges are `169.254.104.0` through `169.254.167.255` and
`198.18.0.0` through `198.18.191.255`. The latter is benchmarking space already
protected by the shared policy compiler, including DNS answer filtering.
A single link-local `/16` cannot hold 16 profiles of 1,024 `/30`s plus DNS,
metadata and reserved addresses. Moving only the additional slots to a second
protected pool avoids changing existing assignments or using public addresses.
Operators must reserve both pools against host/VPN conflicts. Shared-host
Compose/scenario selectors exclude the expansion pool even before links exist;
the isolated Firecracker probe's `198.18.200.0/24` remains outside it.

## Mechanism and runtime evidence

Tests used disposable Docker network/cgroup namespaces on an amd64 Linux Btrfs
workstation. The tool image was
`ghcr.io/secondstack-ai/secondbox/runner-gvisor@sha256:08a9876c38b9bdab91ce72e2e5672d5070cd0f1da1f6d768a0a30db0218d1a1b`;
the test executable and guest agent were built from changed source. The flat
root came from local v0.33.0 release assets. The reviewed `runsc` SHA-512 pin
is release `20260817.0`. No qualification container used the host network.

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
  -test.v -test.count=1 -test.timeout=10m \
  -test.run 'Test(NetworkSlotExpansionQualification|AttributedExecutionNetworkQualification)$'
```

Coverage includes:

- All 16,384 profile/slot pairs: disjoint subnets and names, preserved mappings,
  bank/pool boundaries, source addresses remaining protected destinations and
  DNS answers, concurrent allocation, exhaustion/reuse, and 1,024/1,025 validation.
- 1,024 simultaneous kernel links in profile 15, alongside other profiles'
  legacy, 256-slot, and expanded mappings. Host/namespace routes select the
  expected links. Reconciliation removes only profile 15's links, namespaces,
  policies and orphaned NAT-only table; slot 1023 can then be recreated.
- Attributed relays, host-client denial, listener revocation and startup table
  reclamation at profile/slot pairs `0/0`, `15/255`, `0/256`, `15/999`, `15/1023`.
- Real runsc network policy at slots 0, 255, 256, 999 and 1023: DNS pin admission,
  disjoint-name/private/metadata denial, pin rotation/expiry, NAT and fencing.
  Earlier slots are reserved in memory, without launching additional sentries.
- Backend boot, conformance, supervisor-loss delivery, deny-all routing,
  attachment/crash recovery and selected-image boot.

All checks above passed. Required `just verify-generated`, `just test` (both
Go modules and vet), `just test-contract`, and the live `just test-compose`
smoke suite passed. The gVisor package also passed `go test -race` and vet
with `listener_qualification`; changed shell scripts passed `bash -n`.
The full Go gate used a dedicated disposable PostgreSQL 18.4 database.

The original 256-link check exposed asynchronous veth disappearance after
namespace deletion. Startup now tolerates only the specific already-absent
error, matching ordinary teardown; other errors still fail startup. The real
backend fixture supplies its required image-verification directory.

## Qualification limits

The earlier broad runtime suite failed
`TestQualifiedGVisorAttributedRevocation/exec-end` with a gateway read timeout.
The same failure reproduced on the released allocator plus the fixture
correction; expiry and fence cases passed. This update exercises the affected
network/lifecycle paths without repeating that unchanged known failure.

The full control-plane `just test-scenario` requires configured execution-image,
registry and image-fetcher DNS inputs, which are absent locally. No complete
scenario, Kubernetes or no-KVM host-class qualification is claimed. The real
runtime uses systrap on a KVM-capable workstation. Isolated network checks do
not qualify shared-host Docker firewall behavior or production load/SLOs.
Compute quotas and memory budgets are unchanged.

The optional formatting gate flags unchanged baseline
`runner/internal/runnercontrol/grpc_connector.go`. Changed Go files are formatted.
Use a short persistent `TMPDIR`: long paths exceed Unix socket limits in the
existing attribution tests. Raw logs for this update are retained locally in
`.tmp/gvisor-network-1024/`; the earlier baseline reproduction is in
`.tmp/gvisor-network-slots/`. Temporary build assets, test containers and the
disposable PostgreSQL database are removed at handoff.

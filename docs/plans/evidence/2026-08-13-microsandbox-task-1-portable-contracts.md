# Microsandbox Task 1L portable-contract evidence

Date: 2026-08-13

Task 1L passed on deimos. This checkpoint adds no production Microsandbox composition and makes no
macOS support claim.

## Contract and placement outcomes

- Runner protocol v3 uses provider-neutral `AssetReference`, integer `vcpu_count`, compute-launch
  progress, private backend kind, and exact backend-materialization evidence. Removed fields are
  reserved rather than reused.
- Public OpenAPI, Go, and TypeScript contracts contain neither backend kind nor Firecracker or
  Microsandbox vocabulary.
- Strict materialization manifests bind backend, architecture, runtime and toolchain digests, local
  launch inputs, guest-agent protocol/features, and backend/helper build identity. Microsandbox
  manifests additionally require a digest-pinned OCI source and flat-root digest.
- Firecracker still validates its locally signed artifact manifest and trust anchor before
  advertising a materialization.
- RunnerPool admission transactionally seals an empty pool to the first healthy Runner's backend
  kind and rejects every mismatch. Scheduling rejects a Runner without the exact local
  runtime/toolchain materialization tuple.
- Profiles, quotas, accounting, standard resources, deployment fixtures, and SDKs use integer vCPU
  counts; the removed universal process limit is not retained as a compatibility path.

## Validation

All validation used this worktree and local inputs only.

- `just verify-generated`: passed; descriptor digest verified, 133 TypeScript tests passed, and the
  package dry-run passed.
- `just lint`: passed in both Go modules with zero issues.
- `just test-contract`: passed.
- `just test`: passed against the disposable local PostgreSQL database, including the complete
  integration package and both Go modules.
- `just test-non-kvm`: passed, including generated checks, Go tests/vet, SDK packaging and clean
  consumer checks, Compose smoke, image policy, and local artifact builds.
- `just test-firecracker`: passed on deimos with Firecracker 1.16.1 and the existing locally signed
  v0.3.0 artifact set; the three non-skipping real-KVM smoke tests passed in 25.724 seconds.
- `git diff --check`: passed.

The Firecracker qualification used a transient short `/tmp/q1` symlink into a Btrfs qualification
directory so Unix sockets remained below `sun_path` while rootfs reflinks stayed on the artifact
filesystem. The symlink was removed automatically when the suite exited.

No external repository was mutated, and nothing was pushed or published.

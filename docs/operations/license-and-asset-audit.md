# License and execution-asset audit

SecondBox source is MIT-licensed. The root [LICENSE](../../LICENSE) governs original source, while [THIRD_PARTY_NOTICES.md](../../THIRD_PARTY_NOTICES.md) records dependencies and execution-asset authorities. This audit describes the source tree; a release candidate needs the machine-readable evidence described below.

## Tracked content

The source repository contains no kernel, root filesystem, workspace image, Firecracker executable, jailer executable, guest-agent executable, or runner executable. The two ELF binaries copied from the monorepo baseline were removed because their build metadata was locally modified and did not match the pinned Go toolchain. Release binaries must be rebuilt from source by the artifact job.

The tracked microVM content consists of build scripts, configuration, source locks, signature policy, and tests. Those files are original SecondBox source under MIT unless a file carries a more specific notice.

## Firecracker

`runner/internal/firecracker/firecracker.lock` and the runner image pin Firecracker 1.16.1 and its archive checksum. Firecracker is Apache-2.0. The image build copies the upstream `LICENSE`, `NOTICE`, and `THIRD-PARTY` files beside the installed executable. A released standalone binary package that includes Firecracker must preserve the same three files and identify the verified archive digest in provenance.

## Linux kernel

`runner/scripts/microvm-image/kernel.lock` pins Linux 6.12.94 to a kernel.org source URL and SHA-256 digest. Linux is GPL-2.0-only WITH Linux-syscall-note. A distributed kernel binary must include the applicable license text, build configuration, patches, provenance, and a compliant corresponding-source delivery mechanism. The source repository does not distribute a kernel binary.

## Guest root filesystem

The guest root filesystem is constructed from a dated Debian snapshot plus the package lists and fully pinned top-level Python requirements under `runner/scripts/microvm-image/rootfs`. Debian packages retain their individual licenses, normally recorded in `/usr/share/doc/<package>/copyright`. Python packages retain the license in their source distribution or wheel metadata.

Image ingestion emits the exact Debian package inventory, Python resolved freeze, immutable source identity, input hashes, and collected license/copyright evidence into the prepared root filesystem. It accepts either a digest-qualified OCI source or the explicit dated Debian image definition, and it rejects mutable or ambiguous source inputs before container work. Release assembly must add the SBOM, operator approval evidence, and signed bundle manifest before Firecracker v1 qualification.

## Dependency and runtime inventories

Go modules are fetched from pinned `go.mod` and `go.sum` entries and are not vendored. Their governing licenses are listed in `THIRD_PARTY_NOTICES.md`. Release SBOM and notice generation must resolve the exact module graph for both the control-plane and runner modules.

`scripts/generate-release-sboms.sh` emits CycloneDX module graphs for both Go modules and the npm lockfile, converts the signed guest Debian/Python inventories into a subject-bound CycloneDX document, and uses Syft for each exact binary, release archive, digest-pinned image, and SDK package. Missing Syft or a subject digest blocks that subject and therefore the aggregate gate.

`scripts/generate-license-evidence.sh` copies the root license and notices, records npm license identifiers, collects license, copying, and notice texts for every pinned Go module, verifies the exact npm package notices, and consumes signed guest license inventories. For ordinary digest-pinned images it creates a stopped container without executing it and extracts license, copying, notice, and Debian copyright texts from the exact locally available image. The scratch guest artifact transport image has no independent runtime packages; its license record reuses the signed guest inventories only after the subject manifest binds its image digest to that exact guest bundle digest. An unavailable image, missing Docker content, binding drift, or incomplete guest inventory blocks the subject.

The TypeScript Flue adapter includes a narrow Apache-2.0 compatibility module adapted from `@flue/runtime` 1.0.0-beta.9. Its exact upstream license, source/tag/package hashes, and local adaptation hash are committed beside the module and copied into release license evidence. The full npm package and its transitive graph are not installed.

`scripts/generate-dependency-age-evidence.sh` maps build and runtime dependencies to the exact subjects they affect. It queries `proxy.golang.org`, `registry.npmjs.org`, and the Docker Hub tag API for pinned Go, npm, Dockerfile-base, PostgreSQL, and RustFS versions. It also verifies the exact Firecracker release asset timestamp from GitHub, the exact kernel tarball timestamp from kernel.org, and the dated Debian archive through Debian Snapshot. For a resolved guest subject, it reads `rootfs-python.freeze` from the signed guest archive and queries the exact version on PyPI, using the newest upload timestamp across that version's files and recording all published file SHA-256 digests. Those guest dependency records cover both the bundle and its bound scratch transport image. The report records registry URLs, publication timestamps, resolved identities, computed ages, subject mappings, and ineligible or unavailable records. The minimum release age is four days, and malformed pins, unsupported freeze entries, unsafe archive paths, empty inventories, unavailable registries, and missing publication timestamps fail closed.

## Publication gate

Before publication, release qualification must prove that every binary, container image, kernel, root filesystem, toolchain, and SDK package has an SBOM, notices, checksums, signatures, dependency-age evidence, and provenance tied to its immutable digest. `release/supply-chain-subjects-schema.json`, `release/supply-chain-evidence-schema.json`, and the publication verifier make omitted subjects, digest drift, and missing records fatal. The internal migration tag is not a public release and must not be pushed as one.

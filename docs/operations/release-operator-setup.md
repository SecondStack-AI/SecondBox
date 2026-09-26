# Release operator setup

SecondBox releases are qualified and built locally, then published by one GitHub workflow. The local release flow stages a non-publishable installer candidate, qualifies that exact candidate in disposable virtual machines, and stages the final release from the resulting evidence.

## One-time setup

- Authenticate the local `gh` CLI for `SecondStack-AI/SecondBox`.
- Configure npm trusted publishing for `@secondstack-ai/secondbox` and `.github/workflows/release.yml`.
- Allow that workflow `contents: write`, `packages: write`, and `id-token: write`.
- Give the repository workflow access to the public `control-plane`, `runner`, `installer-tools`, `microvm-artifacts`, `runner-gvisor`, and `gvisor-artifacts` GHCR packages.

The local build host needs Docker Buildx, `jq`, `openssl`, npm, Node.js, Go, and `just`. It must also satisfy the KVM, TUN, reflink-filesystem, and scenario bundle requirements in [external scenario qualification](scenario-qualification.md). Release staging needs the microVM release bundle variables documented by `scripts/release-stage.sh` because the runner consumes that bundle at runtime.

## The microVM bundle and its trust anchor

A release publishes the signed microVM bundle the runner executes. The bundle is built by the [microVM image pipeline](microvm-image-pipeline.md) and signed with the release signing key; staging verifies it against an independently held public anchor before packaging it as the `microvm-artifacts` OCI archive, and the staged artifact manifest records the verified fingerprint as `microvm.signingKeyFingerprint`.

The signing key is RSA — `Config.ValidateMicroVMTrustAnchor` rejects any other algorithm. Keep the private half outside the repository, mode 0600, alongside the release operator's other material and never inside a release artifact, a staged candidate, or command output. Keep the public key and its canonical DER SHA-256 fingerprint next to it, and give staging the public half only:

```sh
export SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR=/absolute/path/to/signed/bundle
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY=/absolute/path/to/manifest-public.pem
export SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY_SHA256="$(
  openssl pkey -pubin -in "$SECONDBOX_RUNNER_MICROVM_RELEASE_PUBLIC_KEY" -outform DER |
    sha256sum | awk '{print $1}'
)"
```

Rebuilding the bundle rotates its identity. The guest agent, `/init`, and the microVM entrypoint live inside the signed rootfs, so any change to them requires a rebuilt, re-signed bundle, and a rebuild changes the runtime and toolchain component-manifest digests that every Profile pins. When a release rotates the bundle it therefore also rotates what operators must install:

- Operators install the new bundle through the runner init flow and pin the fingerprint the release's artifact manifest records. Never pin the `signing.pub` carried inside the bundle — that degrades the anchor to the fingerprint alone, which is exactly what the verifier refuses to rely on.
- A runner refuses any Assignment whose pinned component digests differ from its own locally verified manifest, so the bundle install, the signed asset catalog update, and the standard-resource apply are one coordinated step.
- Announce the rotation in the release notes, naming what requires the new bundle and what still works against an older verified one.

The [v0.12.0 release](../releases/v0.12.0.md) rotated the Firecracker bundle and signing authority. [v0.14.0](../releases/v0.14.0.md) retained that bundle and anchor but required a fresh database and Runner storage root. [v0.17.0](../releases/v0.17.0.md) ships a new signed bundle for its guest change and retains the v0.12.0 anchor; [v0.18.0](../releases/v0.18.0.md) retains that bundle. Consult the target release notes before choosing bundle inputs. Point `SECONDBOX_RUNNER_MICROVM_RELEASE_SOURCE_DIR` at the exact signed bundle named there; do not rebuild a published bundle from other guest sources. A different runtime or toolchain component-manifest digest makes the v1 guided updater reject an update because existing Sandboxes remain pinned to their immutable Profile revisions. The gVisor runner image and artifact transport are built by staging from the repository alone and need no operator input beyond Docker buildx; Microsandbox uses a separate operator-local materialization that is not packaged by this release flow.

## Automated release

Copy `deploy/qualify.env.example` to `~/.config/secondbox/qualify.env` for PR
qualification, and `deploy/release.env.example` to
`~/.config/secondbox/release.env` for releases. Review every path and digest. The Go
suite gate provisions its own disposable PostgreSQL container unless
`SECONDBOX_TEST_DATABASE_URL` names a database you manage.
The release file includes every qualification key and is the sole configuration
used by `just release`; it does not recover inputs from an older release directory.
Run `npm ci --ignore-scripts` once in the checkout. Provision your own named
Buildx builder without changing the caller's selected builder, and set
`RELEASE_BUILDX_BUILDER` in the release file. This selection applies only to
artifact staging; it does not change the scenario harness's local Docker builds:

```sh
docker buildx create --name secondbox-suite-release --driver docker-container \
  --driver-opt image=moby/buildkit@sha256:28a898719c18a33f4e8000685287fa36fd0dd9560c6440227d3a732d79bb41d8
```

Create the configured
`RELEASE_OUTPUT_ROOT` and `SECONDBOX_INSTALLER_EXISTING_WORKSPACE_ROOT` parents;
the latter must be on Btrfs or XFS and traversable by the system libvirt account.
The checked-in examples describe the reviewed release host using public paths;
review and provision every path for another host.

```sh
just qualify                  # PR gates and four independent Firecracker shards
just qualify --tier release   # gates, sharded Firecracker and local gVisor host
just nightly                  # full scenarios and no-KVM gVisor pod suite
# After merging, from a clean checkout of origin/main:
just release VERSION          # lean amd64 release
just release VERSION --full   # alternative: nightly matrix and arm64 images
just release VERSION --resume # recover a gate-only failure using the retained build
```

Release preflight checks source, tag identity, inputs, the pinned Go/protoc and
`/usr/bin/just` toolchain, libvirt availability, absence of `sbq-` domains, and
headroom (100 GiB free in each output/workspace filesystem for a lean release,
200 GiB for `--full`, and enough available memory for a guest plus 4 GiB).
The installer requires 12 GiB of usable guest RAM; configure at least 13312 MiB to allow for kernel reservations.
A 13 GiB guest therefore requires 17 GiB available host memory.
Set `QUALIFY_GATES_FIRST=1` and `QUALIFY_MAX_STACKS=1` to limit scenario concurrency on smaller hosts without omitting scenarios.
It creates a local tag only when absent and
never pushes it. Existing tags must identify HEAD. All three versioned output
directories must be absent; failed output is retained for diagnosis, so archive
or remove only your own failed run's directories before starting a fresh run.
When only a gate failed after the build and every scenario stage passed,
`--resume` reuses `VERSION-build` and its commit-exact evidence, reruns the gates,
and continues from candidate staging.

Qualification, the unbound artifact build, and installer qualification run sequentially so their memory demands do not overlap.
Once qualification and the build pass,
staging binds commit-exact Firecracker and gVisor evidence into the candidate.
The default lean release builds amd64 images and tests one `btrfs_image` guest:
the wizard, reboot recovery, and hello-world microVM. The driver removes the
disposable guest afterwards. `--full` uses nightly qualification, adds the arm64 control-plane
image, and runs all three installer modes, including existing-filesystem
uninstall/resume and the v0.7.2 refusal/recreation boundary. Each selected mode
must satisfy its own required assertions in `tests/installer/vm-scenario.json`.
Guests have separate MACs and loopback SSH forwards. The manifest records the
built image platforms; CLI and deploy binaries retain all four host platforms.
Local gVisor host inputs use `QUALIFY_GVISOR_HOST_BUILD_ROOT`; KVM presence is
allowed for host evidence. Pod evidence is required only by full releases.
`QUALIFY_GUEST_MEMORY_MIB` defaults to 16384; concurrency is at most three and is
capped by available memory after a 4 GiB host reserve. The final stage reuses
the checksummed build and requires installer evidence for exactly those release
bytes. Unbound builds cannot be published.

Each release stage writes a log under `.tmp/release/RUN/`; `timing.md` records
its result and wall clock. Qualification has its own `.tmp/qualify/RUN/` logs and
supports `just qualify --wait RUN`. On hosts that terminate commands when their
terminal disappears, launch the release in a user service:

```sh
mkdir -p .tmp
systemd-run --user --unit="secondbox-suite-release-VERSION-$(date +%s)" --collect \
  --property="WorkingDirectory=$PWD" \
  --property="StandardOutput=file:$PWD/.tmp/release-console.log" \
  --property=StandardError=inherit --setenv="PATH=$PATH" --setenv="HOME=$HOME" \
  --setenv="SECONDBOX_TEST_DATABASE_URL=${SECONDBOX_TEST_DATABASE_URL:-}" \
  /usr/bin/just release VERSION
```

Reserve the KVM host before starting, and the dedicated no-KVM VM for `--full`. Do not stop
another operator's units, domains, Compose stacks, or live deployment to make
room. The automation leaves the no-KVM VM running. Candidate and final outputs
are `RELEASE_OUTPUT_ROOT/VERSION-candidate` and `RELEASE_OUTPUT_ROOT/VERSION`;
`VERSION-build` retains the checksummed intermediate build. The successful
command prints the exact tag-push and `release-upload` commands for explicit
publication. Neither command is run automatically.

## Publication

After `just release VERSION` succeeds, review its final manifest and qualification
logs, then use the exact tag-push and upload continuation it prints. Both must
refer to that successful run's version and output directory. Do not create a
release through independent tag, candidate, or upload commands.

Upload reads the tag's `docs/releases/vVERSION.md` when present, otherwise uses
a placeholder; an optional third `NOTES_FILE` argument supplies the body. It
appends the install and SDK footer and refreshes the draft on retry. The publisher
preserves that body. Use the [release skill](../../.agents/skills/secondbox-release/SKILL.md)
for release decisions, recovery, and verification.

Watch the dispatched workflow with:

```sh
gh run list --workflow release.yml --limit 1
gh run watch --exit-status
```

If publication fails while the release is still a draft, fix the cause and retry
the same upload from the successful staged run. Never move a published tag;
use a new patch version for changed artifacts.

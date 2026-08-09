# Guided single-host installation

`secondbox-deploy install` installs one loopback-only development deployment and one same-host Firecracker Runner on Linux amd64. It consumes one verified public release and writes every selected identity, path, capacity, network, retention, trust anchor, and immutable image reference into a durable install plan and `secondbox.toml`. It does not create production authority, a remote Runner topology, an alternate compute backend, or automatic updates.

## Before starting

The host must use systemd and cgroup v2, have Docker Engine and Compose v2, expose accessible `/dev/kvm` and `/dev/net/tun`, and support hardware virtualization. Btrfs kernel support is required only for the portable filesystem-image choice. The host needs at least 6 logical CPUs and 12 GiB of memory so the control services can retain 2 CPUs and 4 GiB while the release-owned `durable-coding` Profile receives 4 CPUs and 8 GiB. Internet access to GitHub Releases and GHCR is required while installing.

The workspace proposal requires at least 50 GiB for that Profile. The deployment filesystem, normally the invoking user's home filesystem, must separately have at least 11 GiB free for verified release materialization. `/var/lib` must retain at least 16 GiB for control-service data and host backing reserve. The portable Btrfs-image choice starts at a fully allocated 65 GiB image and is offered only when the computed image size is at least that large and `/var/lib` still retains the 16 GiB reserve (at least 81 GiB free in the minimum case).

Run the read-only check first:

```sh
secondbox-deploy install --check
```

The check performs no sudo, download, image pull, directory creation, mount, or service change. It reports all findings in one pass:

- `pass` means the capability is already present;
- `warning` identifies a supported but noteworthy condition;
- `remediable` means the accepted install can perform the bounded change;
- `needs user action` requires the operator to correct the host and rerun;
- `blocked` means this host is outside the qualified matrix.

The command exits nonzero when action or an incompatibility remains. Host facts include only the relevant device, filesystem, route, Docker IPAM allocation, port, capacity, UID, software-version, and connectivity evidence; they exclude credentials and unrelated inventory. The plan uses those routes and allocations to choose distinct guest and Compose backend networks instead of asking Docker to consume an automatic address pool.

## Run and review the wizard

The stable bootstrap coordinate is:

```sh
curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh | sh
```

For an inspect-first flow:

```sh
install_dir=$(mktemp -d)
cd "$install_dir"
curl -fLO https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh
curl -fLO https://github.com/SecondStack-AI/SecondBox/releases/latest/download/SHA256SUMS
grep '  install.sh$' SHA256SUMS | sha256sum -c -
sed -n '1,240p' install.sh
sh install.sh
```

The normal wizard asks for a workspace choice, reviews its conservative resource budget, and asks for final confirmation. `secondbox-deploy install --advanced` additionally exposes the proposed addresses, ports, separate guest and Compose backend CIDRs, DNS upstream, jailer UID range, paths, capacity, and CLI subject identity.

Workspace choices are:

- An existing dedicated, non-root XFS or Btrfs mount with at least 65 GiB available. The mount must permit executable files and device nodes because Firecracker executes and opens KVM/TUN devices inside its per-Instance jail. The installer creates one operation-specific Runner storage tree there for verified execution assets, run state, Workspaces, and local Snapshots.
- A fully allocated Btrfs filesystem image of at least 65 GiB. The image contains the same complete Runner storage tree and is mounted `nosuid` while permitting the execution and device access required by the jailer. It is portable and size-bounded, but its availability still depends on the backing filesystem and its systemd loop mount. Back it up as durable Runner storage, not as a replaceable cache.

The final review lists the release version and manifest, seven digest-pinned OCI images, signing-key fingerprint, expected downloads, disk allocation, the explicit vendor-neutral Firecracker CPU-template choice (`None`), all generated authority categories, exact paths, services, retention, capacity, network settings, CLI upgrade policy, and uninstall behavior. Confirmation creates a mode-`0600` plan and receipt before any secret or host resource exists. The guided single-host topology intentionally exposes the host CPU feature set instead of applying Firecracker's vendor-specific static templates; snapshots remain Runner-local and a running Sandbox never relocates.

## Privileged actions

After confirmation, the installer displays the precise privileged list and invokes sudo once for its private host-preparation entry point. Root independently verifies the accepted plan, invoking user, machine identity, KVM, TUN, cgroups, UID range, filesystem, free space, and create-only paths. Depending on the workspace choice, it may:

- create the declared Runner storage root, invoking-user-owned atomic artifact-publication parent, and root- or Runner-owned state, jail, runtime, network, log, snapshot-template-cache, and Workspace directories;
- fully allocate and format the declared regular Btrfs image with the release-pinned installer-tools image;
- install, verify, enable, and start the exact systemd mount unit;
- prove an actual cross-directory rootfs reflink from the artifact-publication parent into the Runner run directory, mutation isolation, and one matching filesystem identity for assets, run state, and Workspaces.

It never formats a physical block device, installs distribution packages, edits shell profiles, creates jailer accounts, or gives the control plane host privileges. The separately deployed Runner container remains the only privileged compute component and executes `secondbox-runner` directly as PID 1.

## Files, services, and first Sandbox

The plan records the exact operation directory, manifest, secrets, Runner identity, reflink-capable Runner storage root, verified artifact directory, Runner state paths, Workspace, optional filesystem image and mount unit, CLI configuration, and installed binary paths. Before downloading release bytes, the installer checks both binary destinations and the CLI configuration destination. A missing target is created, an exact release binary is adopted, and an older executable is replaced atomically only when its embedded Go main-package and module identities are exactly the corresponding SecondBox CLI. An existing configuration is replaced atomically only when it has the reviewed mode and ownership and strictly decodes as a complete SecondBox session document. Any unrelated file, symbolic link, ownership change, permission change, or malformed configuration is refused without modification. A purged installation removes the reviewed, receipt-managed CLI binaries and configuration even when they replaced an older SecondBox release. The verified assets, run/jail/cache state, and Workspace are sibling subtrees on the selected filesystem; WorkspaceStore remains the only component that resolves paths beneath the Workspace subtree. The installer creates unique application, platform, Runner-enrollment, and Runner-PKI authority in protected referenced files. No secret value enters the plan, receipt, command arguments, or installer output.

It then pulls exactly the control-plane, Runner, microVM-artifact, installer-tools, PostgreSQL, object-store, and object-store-client images by digest; verifies every release object and the fixed microVM bundle allowlist; publishes the artifact directory atomically on Runner storage; generates the explicit manifest; enrolls the Runner; starts Compose; logs in the local CLI; waits for advertised cold-boot capacity; and runs:

```sh
secondbox run durable-coding -- python3 -c 'print("hello from a microVM")'
```

There is no automatic updater. Install a later release only through a separately reviewed future operation; do not replace files or image references behind an existing receipt.

## Resume and diagnose

Every durable stage is recorded. If the process or host stops, use the operation path printed by the installer:

```sh
secondbox-deploy install --resume /absolute/path/to/operation
```

Resume locks the operation, revalidates the plan and host identity, and checks recorded files, modes, digests, image identities, artifact hashes, manifest, Runner identity, Compose project, CLI configuration, and health before skipping work. It refuses a changed or foreign resource instead of recreating an empty Workspace. Verified artifacts and binaries are not re-extracted merely because a later stage failed.

### Recover a pre-v0.4.7 Compose network failure

An operation accepted by v0.4.4 through v0.4.6 does not contain a Compose backend CIDR. If it failed at `Compose startup` with `all predefined address pools have been fully subnetted`, preserve that immutable plan and create the exact project-scoped network before resuming. This procedure applies to that allocation failure and those receipt-compatible installer versions only.

First derive the project identity from the accepted plan and inspect every conflicting source:

```bash
set -euo pipefail

operation=/absolute/path/to/operation
plan="$operation/install-plan.json"
operation_id=$(jq -er '.operationId | select(test("^install_[0-9a-f]{16}$"))' "$plan")
project="secondbox-${operation_id#install_}"
network="${project}_secondbox-backend"
guest_cidr=$(jq -er '.network.guestBridgeCidr' "$plan")

printf 'Guest network: %s\n' "$guest_cidr"
ip -j -4 route show table all | jq -r '.[] | (.dst // "default")'
network_ids=$(docker network ls --quiet --no-trunc)
if [ -n "$network_ids" ]; then
  docker network inspect $network_ids | jq -r '.[].IPAM.Config[]?.Subnet'
fi
```

Choose one canonical RFC1918 `/24` that does not overlap the guest CIDR, any prefix or bare host address from any host routing table, or any Docker IPAM subnet. Review the exact value, then create the Compose-owned network with both required labels:

```bash
set -euo pipefail

operation=/absolute/path/to/operation
plan="$operation/install-plan.json"
operation_id=$(jq -er '.operationId | select(test("^install_[0-9a-f]{16}$"))' "$plan")
project="secondbox-${operation_id#install_}"
network="${project}_secondbox-backend"
compose_cidr=10.0.0.0/24 # replace with the reviewed collision-free /24

docker network create \
  --driver bridge \
  --subnet "$compose_cidr" \
  --label "com.docker.compose.project=$project" \
  --label 'com.docker.compose.network=secondbox-backend' \
  "$network"

docker network inspect "$network" | jq -e \
  --arg project "$project" --arg cidr "$compose_cidr" \
  'length == 1 and
   .[0].Labels["com.docker.compose.project"] == $project and
   .[0].Labels["com.docker.compose.network"] == "secondbox-backend" and
   .[0].IPAM.Config == [{"Subnet": $cidr, "Gateway": .[0].IPAM.Config[0].Gateway}]'

secondbox-deploy install --resume "$operation"
```

If the CIDR review was wrong, roll back only while the network has no attached containers; the check fails closed otherwise:

```bash
set -euo pipefail

operation=/absolute/path/to/operation
plan="$operation/install-plan.json"
operation_id=$(jq -er '.operationId | select(test("^install_[0-9a-f]{16}$"))' "$plan")
project="secondbox-${operation_id#install_}"
network="${project}_secondbox-backend"

docker network inspect "$network" | jq -e '.[0].Containers | length == 0'
docker network rm "$network"
```

Once containers are attached, do not remove the network manually. Use the v0.4.7 bootstrap to run the installer recovery action from its temporary, checksum-verified binary. Do not replace the older receipt-bound binary installed by the failed operation. The recovery action locks and validates the plan, receipt, recorded manifest and binary digests, failed stage, and exact Compose project before tearing down partial containers. It preserves the failed operation's durable paths, verified release assets, generated authority, installed binaries, and retryable receipt:

```bash
set -euo pipefail

operation=/absolute/path/to/operation
curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/download/v0.4.7/install.sh \
  | sh -s -- --recover-compose-network "$operation"
```

After the recovery action succeeds, repeat the collision review and network-creation procedure with a corrected `/24`, then resume the preserved operation. Use the operation's support workflow if receipt-bound teardown fails.

Failures are reported as blocked, needs-action, retryable, or internal with the failed stage and a recovery direction. Preserve the operation directory: its plan and receipt are the audit and recovery boundary. The ordinary bounded support bundle is documented in [observability and diagnostics](observability-and-diagnostics.md); installer failures should additionally retain the redacted preflight report, plan digest, non-secret manifest inspection, Compose/systemd status, bounded Runner logs, filesystem facts, and unauthenticated health response.

Create that installer-specific bounded archive with:

```sh
secondbox-deploy install --support /absolute/path/to/operation \
  --output /secure/path/secondbox-installer-support.tar.gz
```

The archive is create-only and mode `0600`. It contains no plan document, process environment, token, private key, Workspace content, database row, or object-store object. Collection rejects output containing any exact installed secret value or a secret-bearing marker. Review bounded Runner log text before sharing it.

## Uninstall and purge

Ordinary uninstall stops the Compose project and deliberately preserves the manifest, secrets, Runner identity, database and object-store volumes, verified artifacts, binaries, CLI configuration, Workspace, optional Btrfs image and mount unit, and receipt:

```sh
secondbox-deploy uninstall /absolute/path/to/operation
secondbox-deploy install --resume /absolute/path/to/operation
```

Permanent deletion is a separate workflow. Run ordinary uninstall first, inspect the listed targets, then request purge:

```sh
secondbox-deploy uninstall --purge /absolute/path/to/operation
```

Purge requires an interactive typed `PURGE <operation-id>` confirmation. It first removes the exact Compose project's bundled PostgreSQL and object-store volumes, verifies and journals removal of the execution assets while the accepted release manifest remains available, and only then removes the privileged Runner storage root. All deletion remains constrained to exact plan-and-receipt-matched resources through symlink- and mount-confined operations. It refuses broad paths, globs, physical or foreign mounts, altered ownership evidence, and changed targets. The plan and receipt remain as a bounded tombstone.

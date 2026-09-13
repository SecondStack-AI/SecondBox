# SecondBox

**Durable, isolated development sandboxes — as a service you run yourself.**

[![CI](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml/badge.svg)](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SecondStack-AI/SecondBox)](https://github.com/SecondStack-AI/SecondBox/releases/latest)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

SecondBox runs untrusted workloads — AI agents, user code, plugins, CI jobs, long-lived dev environments — inside isolated Sandboxes whose filesystems survive between sessions. The Firecracker backend boots each Sandbox as a microVM on KVM hosts; the gVisor backend serves Linux hosts without KVM, including Kubernetes nodes.

- **Hardware isolation.** On KVM hosts every Sandbox is a Firecracker microVM, not a container. The gVisor backend substitutes a userspace-kernel sentry for hosts without hardware virtualization; its isolation boundary is the sentry, not KVM.
- **Durable workspaces.** A Sandbox keeps its disk across stops, restarts, and generations. Snapshot it and restore in place.
- **Real terminals.** A genuine PTY with raw mode, resize forwarding, and bounded reconnect — not a line-buffered exec loop.
- **Multi-tenant by construction.** Every row is scoped to an opaque tenant and subject reference. Application tokens carry fixed scopes and explicit Profile grants.
- **Immutable Profiles.** Operators fix image, resource defaults and optional ceilings, lifecycle, network, and port policy. Each Sandbox pins the revision resolved at creation.
- **Self-hosted.** One unprivileged control plane, PostgreSQL, and one or more privileged runners you place yourself.

> [!NOTE]
> SecondBox is a **networked control plane**, not an embeddable library. Sandboxes run on separately deployed runners, and a client only ever talks to the control plane over HTTPS. There is no daemonless mode.

## How it works

A **Sandbox** is the durable public resource; the **Instance** running it is replaceable compute fenced to one Sandbox generation. Each Sandbox is placed at creation on one home **Runner**, whose reflink-capable filesystem owns that Sandbox's **Workspace** and local **Snapshots**. Ordinary lifecycle and automatic recovery never relocate it. An operator may relocate a stopped Sandbox with no retained Snapshots through the explicit asynchronous relocation operation.

`secondboxd` stores desired state in PostgreSQL. Workspace bytes stay on the owning Runner except while `secondboxd` forwards a bounded, in-memory stream for an explicit stopped-Sandbox relocation; it never persists those bytes.

## Getting started

### Guided single-host install

The guided installer turns one qualified Linux amd64 systemd host into a loopback-only development deployment with PostgreSQL, the control plane, and one same-host Firecracker Runner. It verifies a published release and records every accepted path and authority decision. With local tenancy selected (the guided default), it creates a Tenant and Subject, configures the platform CLI session for `secondbox run`, and verifies hello-world guest execution. Declining tenancy leaves a platform session and record-only readiness checks.

The host needs Docker Engine with Compose v2, cgroup v2, accessible KVM and TUN devices, hardware virtualization, at least 6 logical CPUs and 12 GiB of memory. Runner storage needs at least 65 GiB: 50 GiB for the `durable-coding` Workspace, approximately 11 GiB for verified execution assets, and a 4 GiB margin. Use a dedicated non-root XFS/Btrfs filesystem with that capacity, or let the installer create a fully allocated Btrfs image of at least 65 GiB; the image choice additionally needs its full allocation plus the reviewed control-service, download, and backing reserves on `/var/lib`. Check the host without changing it:

```sh
secondbox-deploy install --check
```

To fetch the small published bootstrap and run the wizard:

```sh
curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh | sh
```

To update a completed guided deployment after stopping every Sandbox, pass its recorded operation directory to the latest bootstrap:

```sh
curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh \
  | sh -s -- update /absolute/path/to/secondbox-install-operation
```

Run the same command with `update --check` first for read-only compatibility, drift, and staging-capacity validation. The guided updater accepts completed v0.6.0 or newer installations; v0.6.0 is a clean-install boundary, so earlier installations require a fresh deployment with explicit workload migration. Updates preserve the existing PostgreSQL volume, generated authority, Runner identity, Workspaces, Snapshots, storage, ports, and Compose project. The v1 updater rejects releases that change runtime or toolchain bundle digests because existing Sandboxes remain pinned to their immutable Profile revisions.

The bootstrap downloads only the release-pinned Linux amd64 `secondbox-deploy` binary to a temporary directory, verifies its embedded SHA-256 digest, and dispatches the requested install or update operation. It does not invoke sudo or modify the host itself. The installer shows its exact privileged action list before asking sudo to run its narrow host-preparation entry point.

If you do not pipe scripts into a shell, download and inspect the same assets first:

```sh
install_dir=$(mktemp -d)
cd "$install_dir"
curl -fLO https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh
curl -fLO https://github.com/SecondStack-AI/SecondBox/releases/latest/download/SHA256SUMS
grep '  install.sh$' SHA256SUMS | sha256sum -c -
less install.sh
sh install.sh
```

See [guided single-host installation](docs/operations/guided-single-host-install.md) for every prerequisite, wizard choice, created resource, recovery command, and durability boundary.

### Other deployment paths

The guided path is deliberately Linux amd64, same-host, loopback-only, and development-mode. It does not replace these separate workflows:

- Download individual release binaries or SDKs from the [latest release](https://github.com/SecondStack-AI/SecondBox/releases/latest) when you only need a client.
- Follow [deployment and runtime operations](docs/operations/deployment.md) for production authority, remote Runners, or a manually reviewed same-host topology.
- For a manually reviewed same-host Runner declaration, start with `secondbox-deploy runner-template`; it emits every required Runner-host path and authority field without installing anything.
- Use `just deploy-development-up .tmp/secondbox-development` for control-plane-only source-checkout development. That topology has synthetic development artifact identity and cannot execute a Sandbox until a real Runner and verified assets are configured.
- Follow the [microVM image pipeline](docs/operations/microvm-image-pipeline.md) and [release distribution](docs/operations/release-distribution.md) when producing or independently materializing execution assets.

### Control-plane-only start

```sh
just deploy-development-up .tmp/secondbox-development
```

This creates one private, versioned `secondbox.toml`, generates unique referenced secrets, compiles a protected environment transport, and starts the reviewed loopback PostgreSQL and control-plane topology. The generated environment is never operator input. This topology is useful for control-plane development and API work, but it cannot execute a Sandbox. Read [deployment and runtime operations](docs/operations/deployment.md) before exposing the API, configuring production, or enrolling a Runner.

### Install the CLI

Use the release binary above, or build the current checkout from source:

```sh
go build -o ./dist/secondbox ./cmd/secondbox
```

### Log in once

```sh
secondbox platform login \
  --url https://secondbox.example.com \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref acme \
  --subject-ref alice
```

Credentials are verified against the deployment before anything is written, then stored at mode `0600`. Every later command resolves them from the first source that has them: an explicit flag, then `SECONDBOX_URL` / `SECONDBOX_TOKEN` / `SECONDBOX_TENANT_REF` / `SECONDBOX_SUBJECT_REF`, then that file. `secondbox whoami` shows what resolved and from where; it never prints the token.

### Run something

```sh
secondbox run durable-coding -- python3 -c 'print("hello from a microVM")'
```

## Using the CLI

### Run with the resources you need

`run` creates a Sandbox, waits for readiness, executes a command, and deletes
it. Add `--keep` to retain its Workspace and report its identifier:

```sh
secondbox run durable-coding --name mybox --keep -- true
secondbox run durable-coding --cpus 2 --memory 4GiB --disk 20GiB -- python3 -c 'print("hello")'
secondbox get mybox
```

| Size | vCPUs | Memory | Workspace |
|---|---:|---:|---:|
| `small` | 1 | 1 GiB | 4 GiB |
| `medium` | 2 | 4 GiB | 16 GiB |
| `large` | 4 | 8 GiB | 50 GiB |

Explicit `--cpus`, `--memory`, and `--disk` override individual preset axes.
Byte sizes accept `GiB`, `MiB`, `KiB`, `g`, `m`, `k` (case-insensitive binary
units), or plain bytes. Without a preset, omitted axes use the Profile values.
Requested disk capacity rounds up to the next power of two before admission,
so 20 GiB becomes 32 GiB, except that a request within the Profile's disk
ceiling never fails because of rounding: it resolves to the ceiling itself
when the rounded value would exceed it, which is why `--size large` still
fits standard `durable-coding` at 50 GiB. Omitted disk uses the Profile
default unchanged.

Without `resourceCeiling`, Profile resource defaults are also ceilings.
Operators may publish explicit bounds for all three axes, using `null` to
leave an axis bounded only by Tenant/Subject quota and Runner admission.
The [flexible Profile example](examples/resources/durable-coding-flexible.json)
permits unbounded CPU/memory and up to 256 GiB of Workspace capacity. Requests
above a finite bound fail with `resources_exceed_profile`, never shrink.
`snapshot_resume` Profiles require their exact default size and reject changes
with `resources_fixed_by_profile`. `get` and the TTY retained Sandbox summary
show the resolved resources.

For creation without an initial command, use `create`. It returns the admitted
Operation immediately; the Profile chooses the initial state. Inspect readiness
before executing in it, or use `run --keep -- true` to create and wait:

```sh
secondbox create durable-coding --size small --name worker
secondbox get worker
secondbox ls
```

### Work with files, commands, and ports

A new Workspace starts empty. Copy your files explicitly; your local checkout
is never mounted or copied implicitly. These examples assume `./src` exists:

```sh
secondbox cp -r ./src mybox:/workspace/src
secondbox exec mybox --shell -- 'printf "hello\n" > /workspace/out.txt'
secondbox cp mybox:/workspace/out.txt ./out.txt
secondbox ls-files mybox /workspace
secondbox shell mybox
secondbox ports forward mybox 3000
```

The port forward listens on `127.0.0.1:3000` until interrupted; run a server on
Sandbox port 3000 in another terminal first. Use `8080:3000` for different local
and remote ports, or `--bind` to select the local address. The Profile must
permit the remote port. Files use `sandbox:/workspace/path` operands, and
`cp -r` recursively copies directories.

Friendly verbs taking a Sandbox accept its name or `sbx_…` identifier. Names
use the reserved `secondbox.dev/name` metadata key, unique per Tenant and
Subject until deletion, and resolve through the API from any machine.
`run` and `create` take a Profile instead.

Guest stdout and stderr stay separate, and the guest's exit status becomes the
CLI exit status. `run` deletes its Sandbox even after a failed command unless
`--keep` is set. `--shell` accepts one shell command; `--stdin` forwards buffered
input. `run PROFILE --tty` creates an interactive session, and `--keep` retains
it after disconnection. `shell` attaches to an existing Sandbox and manages
its generation and Lease.

### Stop, snapshot, and reuse

Snapshots capture a stopped Sandbox's durable Workspace. Keep the source's
disk capacity when cloning; CPU and memory may differ within the target
Profile's ceilings:

```sh
secondbox stop mybox
secondbox snapshot mybox --name with-deps
secondbox snapshots mybox
secondbox run durable-coding --from mybox/with-deps -- cat /workspace/out.txt
secondbox start mybox
secondbox stop mybox
secondbox rm mybox
```

`start`, `stop`, and `rm` wait for their terminal state; `--no-wait` returns the
admitted Operation instead. `rm` asks for confirmation only on a TTY, and
`--force` skips it. `list` aliases `ls`; `delete` aliases `rm`. `ls` includes
all states returned by the API and accepts `--name` for an exact name filter.

`run` and `create` accept `--from sandbox/snapshot-name` or an opaque `snp_…`
identifier. `restore SANDBOX REF` restores into a stopped Sandbox, while
`snapshot rm REF` deletes a Snapshot; both return an admitted Operation.
Snapshot names are unique among ready Snapshots in the source Sandbox.

To install dependencies under `/workspace` and reuse them, follow the
[golden-snapshot workflow](docs/operations/sdk-cli-and-flue.md#golden-snapshot-workflow).
It includes a reviewed operator Profile example with registry HTTPS access;
the standard `durable-coding` bundle allows only the platform gateway.

### Terminal presentation and scripts

Global presentation flags precede the command:

```sh
secondbox --output plain --color never ls
secondbox --output json get mybox
```

Eligible terminals receive compact summaries and bounded tables; pipes retain
API JSON. `--output plain` selects an unstyled human view, and `--output json`
preserves API response bytes. For lifecycle mutations, JSON is the admitted
Operation even when the human view waits and shows the resulting resource.
Guest streams remain raw; `run --json` and `exec --json` explicitly request the
ExecOutcome. `NO_COLOR` disables automatic color; `--accessible` selects
accessible prompts. See the [CLI output contract](docs/operations/cli-output-contract.md)
for every classification.

### Everything else

Generic aliases remain thin transport over the API. Use repeatable `--path`,
`--query`, and `--header` pairs, and `--body` with a file or `-` for stdin:

```sh
secondbox profiles list
secondbox profiles get --path profileName=durable-coding
secondbox operation listSandboxes --query limit=20
```

`operation OPERATION_ID` reaches any published route directly. Generic mutations
require the headers specified by that operation; friendly verbs and SDK helpers
compute their own idempotency, revision, and generation values.

Full reference: [SDK, CLI, and Flue quick starts](docs/operations/sdk-cli-and-flue.md).

## SDKs

Go and TypeScript share one handwritten composition layer over generated transports and wire types: idempotency keys, bounded-wait looping, lease keepers that renew in the background, outcome decoding, and `run`.

```go
client, _ := secondboxclient.NewSecondBoxSubjectClient(
    "https://secondbox.example.com", token, "acme", "alice", http.DefaultClient)

ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
defer cancel()

handle, outcome, err := client.Run(ctx, secondboxclient.RunRequest{
    Profile: "durable-coding",
    Command: secondboxclient.Command{ArgvCommand: &secondboxclient.ArgvCommand{
        Mode: "argv", Executable: "python3", Arguments: []string{"-c", "print('hello')"},
    }},
    DeadlineMilliseconds: 30_000,
    MaximumOutputBytes:   1 << 20,
})
fmt.Print(string(outcome.Result.Stdout))
_ = handle // Run never deletes; disposal is yours
```

<details>
<summary><b>TypeScript</b></summary>

```ts
import { SecondBox, SecondBoxClient } from "@secondstack-ai/secondbox";

const api = new SecondBox(
  new SecondBoxClient("https://secondbox.example.com", token, fetch, "acme", "alice"),
);

const { handle, result } = await api.run({
  profile: "durable-coding",
  command: {
    mode: "argv",
    executable: "python3",
    arguments: ["-c", "print('hello')"],
  },
  deadlineMilliseconds: 30_000,
  maximumOutputBytes: 1_048_576,
  readyTimeoutMilliseconds: 300_000,
});

if (result.kind === "exited") process.stdout.write(result.stdout);
```

</details>

`Run` never deletes the Sandbox it created in either client — disposal stays your decision.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/secondbox` | the CLI |
| `cmd/secondboxd` | unprivileged control plane |
| `runner` | privileged Firecracker runner and guest agent |
| `contracts` | canonical public, runner, and guest-agent protocols |
| `internal` | domain, API, scheduling, reconciliation, persistence |
| `migrations/postgres` | database migration lineage |
| `sdk` | Go and TypeScript clients |
| `deploy` | Compose, systemd, and deployment examples |
| `docs/design` | architecture and compatibility contracts |
| `docs/operations` | installation, backup, diagnostics |

## Validation

The portable gate needs no KVM and is what CI runs:

```sh
just test-non-kvm
```

Firecracker validation requires a dedicated Linux host with KVM and the configured assets:

```sh
just test-firecracker
```

The external scenario gate joins the HTTP API, PostgreSQL, the runner protocol, and real Firecracker guests. It needs a self-hosted Linux x86-64 machine with writable KVM and TUN devices, cgroup v2, a separately verified signed microVM bundle, and an XFS or Btrfs workspace root with reflink support:

```sh
SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1 just test-scenario
```

See [scenario qualification](docs/operations/scenario-qualification.md) for optional end-to-end testing and timing budgets. Every commit admitted to `main` must pass the GitHub-hosted CI workflow. Releases are built locally, uploaded to a private draft, and published as stable GitHub, GHCR, and npm artifacts without rebuilding. See [release operator setup](docs/operations/release-operator-setup.md).

## Security

Runner connections require TLS 1.3, a CA-signed certificate identifying the Runner, and a pre-shared Runner credential. The HTTP API accepts the deployment-wide platform token for operators, and explicitly configured application authorities bound to fixed tenant and subject references, exact operation scopes, and named Profile grants. None of these authorities are interchangeable.

Loss of an unbacked home-runner workspace filesystem loses that Sandbox: PostgreSQL recovery cannot reconstruct runner-local data. Back up each Runner's stable identity and workspace root as one consistent unit — see [backup and recovery](docs/operations/backup-and-restore.md) and the [threat model](docs/design/threat-model.md).

## License

MIT. Third-party components and execution assets retain their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

# SecondBox

**Self-hosted, isolated Sandboxes with durable workspaces.**

[![CI](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml/badge.svg)](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SecondStack-AI/SecondBox)](https://github.com/SecondStack-AI/SecondBox/releases/latest)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

SecondBox runs untrusted workloads — AI agents, user code, plugins, CI jobs, long-lived dev environments — inside isolated Sandboxes whose filesystems survive between sessions. The Firecracker backend boots each Sandbox as a microVM on KVM hosts; the gVisor backend serves Linux hosts without KVM, including Kubernetes nodes.

- **Workload isolation.** Firecracker uses hardware virtualization on KVM hosts. gVisor uses a userspace-kernel sentry on hosts without KVM.
- **Durable workspaces.** A Sandbox keeps its disk across stops, restarts, and generations. Snapshot it and restore in place.
- **Interactive terminals.** PTYs support raw mode, resize forwarding, and bounded reconnect.
- **Scoped access.** Application-owned resources belong to one Tenant and Subject. Application tokens carry fixed scopes and explicit Profile grants.
- **Immutable Profiles.** Operators fix image, resource defaults and optional ceilings, lifecycle, network, and port policy. Each Sandbox pins the revision resolved at creation.
- **Self-hosted.** One unprivileged control plane, PostgreSQL, and one or more privileged runners you place yourself.

> [!NOTE]
> SecondBox is a network service with separately deployed Runners. Clients use the control-plane API; authorities explicitly granted direct Port transport also connect to the admitted Runner endpoint.

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

**v0.16.0 updates a v0.15.0 deployment in place: it keeps Runner protocol generation 5, the database schema, and the signed fixed-Profile guest bundle.**
Stop active Sandboxes and retain a database and Runner-storage backup before updating; see the [v0.16.0 release notes](docs/releases/v0.16.0.md).
Deployments older than v0.15.0 must first cross the boundary in the [v0.15.0 release notes](docs/releases/v0.15.0.md), and earlier release boundaries still apply.

To update a compatible completed guided deployment after stopping every Sandbox, pass its recorded operation directory to the latest bootstrap:

```sh
curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh \
  | sh -s -- update /absolute/path/to/secondbox-install-operation
```

For a compatible target, run `update --check` first to validate compatibility,
drift, and staging capacity. Updates preserve recorded authority, Runner identity,
and durable data. Database, Runner protocol, and execution-bundle changes can
require recreation; the [update guide](docs/operations/guided-single-host-install.md#update-a-completed-installation)
records those boundaries.

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
secondbox run durable-coding --image registry.example/secondbox/agent:stable -- python3 -c 'print("hello from SecondBox")'
```

## Using the CLI

### Create and size a Sandbox

`run` creates a Sandbox, waits for readiness, executes a command, and deletes
it. Add `--keep` to retain its Workspace and report its identifier:

```sh
secondbox run durable-coding --image registry.example/secondbox/agent:stable --name mybox --keep -- true
secondbox run durable-coding --image registry.example/secondbox/agent:stable --cpus 2 --memory 4GiB --disk 20GiB -- python3 -c 'print("hello")'
secondbox get mybox
```

Use `--size small|medium|large` for client presets, or request individual axes
within the Profile's ceilings, Tenant/Subject quota, and Runner capacity.
Disk requests round up, capped by a finite Profile ceiling; the resolved
allocation appears in `get`. Snapshot-resume Profiles require their exact shape.
See [resource sizing](docs/operations/sdk-cli-and-flue.md#resource-sizes-and-retained-sandboxes)
for presets, rounding, and operator examples.

For creation without an initial command, `create` returns the admitted Operation
immediately. The Profile determines the initial state; check readiness before exec:

```sh
secondbox create durable-coding --image registry.example/secondbox/agent:stable --size small --name worker
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
secondbox run durable-coding --image registry.example/secondbox/agent:stable --from mybox/with-deps -- cat /workspace/out.txt
secondbox start mybox --image registry.example/secondbox/agent:stable
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

Go and TypeScript each provide a handwritten composition layer over generated transports and wire types: idempotency keys, bounded-wait looping, lease keepers that renew in the background, outcome decoding, and `run`.

```go
client, err := secondboxclient.NewSecondBoxSubjectClient(
    "https://secondbox.example.com", token, "acme", "alice", http.DefaultClient)
if err != nil {
    log.Fatal(err)
}

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
// Run retains any created Sandbox; the caller owns cleanup, including on error.
_ = handle
if err != nil {
    log.Fatal(err)
}
fmt.Print(string(outcome.Result.Stdout))
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
| `runner` | privileged compute backends, WorkspaceStore, and guest agent |
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

See [scenario qualification](docs/operations/scenario-qualification.md) for host setup, automated `just qualify` tiers, and timing budgets. Runner protocol, lifecycle reconciliation, and Workspace durability changes require the scenario gate. Every commit admitted to `main` must pass the GitHub-hosted CI workflow. Releases are built locally, uploaded to a private draft, and published as stable GitHub, GHCR, and npm artifacts without rebuilding. See [release operator setup](docs/operations/release-operator-setup.md).

## Security

Runner connections require TLS 1.3, a CA-signed certificate identifying the Runner, and a pre-shared Runner credential. The HTTP API accepts the deployment-wide platform token for operators, persisted tenant-controller credentials for delegated management, and persisted application credentials bound to fixed Tenant/Subject references, exact operation scopes, and named Profile grants. None of these authorities are interchangeable.

Loss of an unbacked home-runner workspace filesystem loses that Sandbox: PostgreSQL recovery cannot reconstruct runner-local data. Back up each Runner's stable identity and workspace root as one consistent unit — see [backup and recovery](docs/operations/backup-and-restore.md) and the [threat model](docs/design/threat-model.md).

## Documentation

Use the [documentation index](docs/README.md) for architecture, operations, and
SDK references. [Plans](docs/plans/README.md) lists unfinished work separately
from completed implementations and historical qualification evidence.

## License

MIT. Third-party components and execution assets retain their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

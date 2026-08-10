# SecondBox

**Durable, isolated development sandboxes — as a service you run yourself.**

[![CI](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml/badge.svg)](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SecondStack-AI/SecondBox)](https://github.com/SecondStack-AI/SecondBox/releases/latest)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

SecondBox runs untrusted workloads — AI agents, user code, plugins, CI jobs, long-lived dev environments — inside Firecracker microVMs whose filesystems survive between sessions.

- **Hardware isolation.** Every Sandbox is a Firecracker microVM, not a container.
- **Durable workspaces.** A Sandbox keeps its disk across stops, restarts, and generations. Snapshot it and restore in place.
- **Real terminals.** A genuine PTY with raw mode, resize forwarding, and bounded reconnect — not a line-buffered exec loop.
- **Multi-tenant by construction.** Every row is scoped to an opaque tenant and subject reference. Application tokens carry fixed scopes and explicit Profile grants.
- **Immutable Profiles.** Operators fix image, resources, lifecycle, network, and port policy. Each Sandbox pins the revision resolved at creation.
- **Self-hosted.** One unprivileged control plane, PostgreSQL, and one or more privileged runners you place yourself.

> [!NOTE]
> SecondBox is a **networked control plane**, not an embeddable library. Sandboxes run on separately deployed runners, and a client only ever talks to the control plane over HTTPS. There is no daemonless mode.

## How it works

A **Sandbox** is the durable public resource; the **Instance** running it is replaceable compute fenced to one Sandbox generation. Each Sandbox is placed at creation on one home **Runner**, whose reflink-capable filesystem owns that Sandbox's **Workspace** and local **Snapshots**. Ordinary lifecycle and automatic recovery never relocate it. An operator may relocate a stopped Sandbox with no retained Snapshots through the explicit asynchronous relocation operation.

`secondboxd` stores desired state in PostgreSQL. Workspace bytes stay on the owning Runner except while `secondboxd` forwards a bounded, in-memory stream for an explicit stopped-Sandbox relocation; it never persists those bytes.

## Getting started

### Guided single-host install

The guided installer turns one qualified Linux amd64 systemd host into a loopback-only development deployment with PostgreSQL, the control plane, and one same-host Firecracker Runner. It verifies a published release, records every accepted path and authority decision, and finishes by running a hello-world command inside a microVM.

The host needs Docker Engine with Compose v2, cgroup v2, accessible KVM and TUN devices, hardware virtualization, at least 6 logical CPUs and 12 GiB of memory. Runner storage needs at least 65 GiB: 50 GiB for the `durable-coding` Workspace, approximately 11 GiB for verified execution assets, and a 4 GiB margin. Use a dedicated non-root XFS/Btrfs filesystem with that capacity, or let the installer create a fully allocated Btrfs image of at least 65 GiB; the image choice additionally needs its full allocation plus the reviewed control-service, download, and backing reserves on `/var/lib`. Check the host without changing it:

```sh
secondbox-deploy install --check
```

To fetch the small published bootstrap and run the wizard:

```sh
curl -fsSL https://github.com/SecondStack-AI/SecondBox/releases/latest/download/install.sh | sh
```

The bootstrap downloads only the release-pinned Linux amd64 `secondbox-deploy` binary to a temporary directory, verifies its embedded SHA-256 digest, and starts `secondbox-deploy install`. It does not invoke sudo or modify the host itself. The installer shows its exact privileged action list before asking sudo to run its narrow host-preparation entry point.

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
secondbox login \
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

### Terminal presentation and scripts

Interactive terminals receive compact summaries, width-aware tables, and
lifecycle status on stderr. Pipes and redirects retain the existing machine
bytes. Global presentation flags precede the command:

```sh
secondbox --output plain --color never sandboxes list
secondbox --output json sandboxes list | jq '.items[] | .id'
secondbox --accessible login
```

`--output auto|json|plain` chooses automatic TTY presentation, original JSON,
or an unstyled human view. `--color auto|always|never` controls ANSI color;
`NO_COLOR` disables automatic color, and `SECONDBOX_ACCESSIBLE=1` is equivalent
to `--accessible`. Raw file/log responses, generic `operation` output,
Docker Compose output, and all guest streams ignore human rendering. See the
[complete CLI output contract](docs/operations/cli-output-contract.md) before
using a command in automation.

### One-off commands

`run` creates a Sandbox, waits for it, runs one command, and deletes it:

```sh
secondbox run durable-coding -- python3 -c 'print("hello")'
secondbox run durable-coding --shell -- 'ls -la /workspace && whoami'
echo 'piped in' | secondbox run durable-coding --stdin -- cat
```

The guest's stdout and stderr land on your two streams, unmerged, and **its exit status becomes the CLI's exit status** — so `secondbox run … -- false` exits 1 and prints nothing of its own, exactly like a local command.

### Named Sandboxes

```sh
# Create one and keep it
secondbox run durable-coding --name my-box --keep -- true

# Address it by name from any machine
secondbox exec my-box --shell -- 'printf "hello from my-box\n" > /workspace/hello.txt'
secondbox exec my-box -- cat /workspace/hello.txt
secondbox shell my-box
```

Names are the reserved metadata key `secondbox.dev/name`, unique per tenant and subject and resolved **server-side** — so the same name works from anywhere, with nothing cached locally. A deleted Sandbox releases its name.

A Sandbox created without a source Snapshot starts with its own empty durable Workspace. Populate `/workspace` through the File API or guest commands; SecondBox never mounts or copies the caller's host checkout implicitly.

`run`, `exec`, and `shell` accept a name or an opaque `sbx_…` identifier, telling them apart by the identifier prefix. The transport-level commands below take the identifier only; `secondbox sandboxes list` shows both.

### Interactive shell

```sh
secondbox run durable-coding --tty              # throwaway shell, deleted on exit
secondbox run durable-coding --tty -- /bin/bash # choose the shell
secondbox shell my-box                              # attach to one that already exists
secondbox shell my-box --command /bin/bash --detachable
```

`run --tty` is the `docker run -it --rm` shape: it creates a Sandbox, waits for it, drops you into a terminal, and deletes it when you disconnect — including on a dropped connection, since the Sandbox exists only for that session. Add `--keep` to retain it and reconnect later with `secondbox shell`.

`shell` resolves the name, applies the Sandbox's current generation, acquires and renews a Lease for the session, and releases it on exit. You get a real PTY: raw mode, local dimensions, `SIGWINCH` forwarding, byte-exact binary I/O, and your terminal restored on exit, cancellation, or transport failure.

Every value it supplies is an overridable default — pass `--lease`, `--generation`, or `--session` and yours wins.

### Everything else

The remaining commands are thin transport over the published API — repeatable `--path`, `--query`, and `--header` pairs, and `--body` taking a file or `-`:

```sh
secondbox sandboxes list

secondbox files read --path sandboxId=sbx_123 --query path=/workspace/out.txt \
  --header SecondBox-Generation=4

secondbox snapshots create --path sandboxId=sbx_123 \
  --header 'If-Match="revision-5"' --header Idempotency-Key=$(uuidgen) \
  --body ./snapshot.json

secondbox exec stream --sandbox sbx_123 --generation 4 \
  --idempotency-key $(uuidgen) --request ./stream.json
```

Routes that mutate a Sandbox require both `Idempotency-Key` and an `If-Match` revision validator; `secondbox sandboxes get` reports the current revision.

`secondbox operation <operationId>` reaches any route in the table directly. Local operator commands — `logs tail`, `logs follow`, `diagnostics bundle`, `timings summary` — need no API credentials for the log routes.

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

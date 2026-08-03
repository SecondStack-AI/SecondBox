# SecondBox

**Durable, isolated development sandboxes — as a service you run yourself.**

[![CI](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml/badge.svg)](https://github.com/SecondStack-AI/SecondBox/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

SecondBox runs untrusted workloads — AI agents, user code, plugins, CI jobs, long-lived dev environments — inside Firecracker microVMs whose filesystems survive between sessions.

- **Hardware isolation.** Every Sandbox is a Firecracker microVM, not a container.
- **Durable workspaces.** A Sandbox keeps its disk across stops, restarts, and generations. Snapshot it and restore in place.
- **Real terminals.** A genuine PTY with raw mode, resize forwarding, and bounded reconnect — not a line-buffered exec loop.
- **Multi-tenant by construction.** Every row is scoped to an opaque tenant and subject reference. Application tokens carry fixed scopes and explicit Profile grants.
- **Immutable Profiles.** Operators fix image, resources, lifecycle, network, and port policy. Each Sandbox pins the revision resolved at creation.
- **Self-hosted.** One unprivileged control plane, PostgreSQL, S3-compatible storage, and one or more privileged runners you place yourself.

> [!NOTE]
> SecondBox is a **networked control plane**, not an embeddable library. Sandboxes run on separately deployed runners, and a client only ever talks to the control plane over HTTPS. There is no daemonless mode.

## How it works

A **Sandbox** is the durable public resource; the **Instance** running it is replaceable compute fenced to one Sandbox generation. Each Sandbox is pinned at creation to one home **Runner**, whose reflink-capable filesystem owns that Sandbox's **Workspace** and local **Snapshots**. A Sandbox never relocates.

`secondboxd` stores desired state in PostgreSQL and immutable **Artifacts** in S3-compatible storage. It never transports workspace bytes — those stay on the runner that owns them.

## Getting started

### 1. Deploy a control plane

```sh
just deploy-development-up .tmp/secondbox-development
```

This creates one private, versioned `secondbox.toml`, generates unique referenced secrets, compiles a protected environment transport, and starts the reviewed loopback PostgreSQL, object-store, and control-plane topology. The generated environment is never operator input. Read [deployment and runtime operations](docs/operations/deployment.md) before exposing the API, configuring production, or enrolling a Runner.

### 2. Give it somewhere to run

A control plane with no runner admits Sandboxes that can never be placed. You need a RunnerPool, an enrolled Runner, and a Profile that pins verified execution assets. SecondBox ships two built-in Profiles, `agent-compartment` and `coding-environment`, whose pool and bundle digests you supply explicitly — see [built-in Profiles](docs/operations/deployment.md#built-in-profiles) and [the Firecracker runtime](docs/operations/firecracker-runtime.md).

### 3. Install the CLI

```sh
go build -o ./dist/secondbox ./cmd/secondbox
```

### 4. Log in once

```sh
secondbox login \
  --url https://secondbox.example.com \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref acme \
  --subject-ref alice
```

Credentials are verified against the deployment before anything is written, then stored at mode `0600`. Every later command resolves them from the first source that has them: an explicit flag, then `SECONDBOX_URL` / `SECONDBOX_TOKEN` / `SECONDBOX_TENANT_REF` / `SECONDBOX_SUBJECT_REF`, then that file. `secondbox whoami` shows what resolved and from where; it never prints the token.

### 5. Run something

```sh
secondbox run coding-environment -- python3 -c 'print("hello from a microVM")'
```

## Using the CLI

### One-off commands

`run` creates a Sandbox, waits for it, runs one command, and deletes it:

```sh
secondbox run coding-environment -- python3 -c 'print("hello")'
secondbox run coding-environment --shell -- 'ls -la /workspace && whoami'
echo 'piped in' | secondbox run coding-environment --stdin -- cat
```

The guest's stdout and stderr land on your two streams, unmerged, and **its exit status becomes the CLI's exit status** — so `secondbox run … -- false` exits 1 and prints nothing of its own, exactly like a local command.

### Named Sandboxes

```sh
# Create one and keep it
secondbox run coding-environment --name my-box --keep -- true

# Address it by name from any machine
secondbox exec my-box -- go test ./...
secondbox exec my-box --shell -- 'cd /workspace && make build'
secondbox shell my-box
```

Names are the reserved metadata key `secondbox.dev/name`, unique per tenant and subject and resolved **server-side** — so the same name works from anywhere, with nothing cached locally. A deleted Sandbox releases its name.

`run`, `exec`, and `shell` accept a name or an opaque `sbx_…` identifier, telling them apart by the identifier prefix. The transport-level commands below take the identifier only; `secondbox sandboxes list` shows both.

### Interactive shell

```sh
secondbox run coding-environment --tty              # throwaway shell, deleted on exit
secondbox run coding-environment --tty -- /bin/bash # choose the shell
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
    Profile: "coding-environment",
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
  profile: "coding-environment",
  command: "python3 -c 'print(\"hello\")'",
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

The external scenario gate joins the HTTP API, PostgreSQL, object storage, the runner protocol, and real Firecracker guests. It needs a self-hosted Linux x86-64 machine with writable KVM and TUN devices, cgroup v2, a separately verified signed microVM bundle, and an XFS or Btrfs workspace root with reflink support:

```sh
SECONDBOX_REQUIRE_QUALIFIED_SCENARIO=1 just test-scenario
```

See [scenario qualification](docs/operations/scenario-qualification.md) for setup, evidence, and timing budgets. Every commit admitted to `main` must pass the GitHub-hosted CI workflow. A release is a Git tag on a commit that also passed the separate scenario qualification workflow on a qualified host.

## Security

Runner connections require TLS 1.3, a CA-signed certificate identifying the Runner, and a pre-shared Runner credential. The HTTP API accepts the deployment-wide platform token for operators, and explicitly configured application authorities bound to fixed tenant and subject references, exact operation scopes, and named Profile grants. None of these authorities are interchangeable.

Loss of an unbacked home-runner workspace filesystem loses that Sandbox: PostgreSQL or S3 recovery alone is not sufficient. Back up each Runner's stable identity and workspace root as one consistent unit — see [backup and recovery](docs/operations/backup-and-restore.md) and the [threat model](docs/design/threat-model.md).

## License

MIT. Third-party components and execution assets retain their own licenses; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

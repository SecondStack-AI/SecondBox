# SDK, CLI, and Flue quick starts

The versioned OpenAPI contract is canonical. Go and TypeScript operation tables and wire types are generated from it, while their HTTP mechanics and composition helpers remain handwritten. Both layers add structured errors, explicit operation polling, caller-owned Sandbox handles, and data-plane helpers. The Go helpers own authenticated `secondbox.exec.v1` and `secondbox.terminal.v1` WebSocket attachments. The TypeScript helpers apply the same sequencing and terminal rules over injected connectors.

## Composition helpers

Both clients align their handwritten composition layers, so a caller states intent rather than sequencing requests:

| Concern | Go | TypeScript |
| --- | --- | --- |
| Generate a request key | `NewIdempotencyKey` | `newIdempotencyKey` |
| Render an If-Match validator | `RevisionETag` | `revisionETag` |
| Read a typed problem code | `ProblemCodeOf` | `problemCodeOf` |
| Decode a terminal outcome | `DecodeExecOutcome` | `decodeExecOutcome` |
| Wait past the per-request bound | `SandboxHandle.WaitFor` | `SandboxHandle.waitFor` |
| Create and hold a Sandbox | `CreateSandbox` | `createSandbox` |
| Hold a Lease active | `KeepLease`, `LeaseKeeper` | `keepLease`, `LeaseKeeper` |
| Create, wait, and execute | `Run` | `run` |

An idempotency key is generated whenever a caller supplies none, and a supplied key is always preserved. `waitFor` issues repeated bounded waits because one `waitForSandbox` request is capped at 60 seconds, and refreshes when the service reports `wait_expired`. A `LeaseKeeper` renews against the expiry the service actually granted rather than the duration requested, since the pinned Profile bounds Lease length; closing one reports a renewal failure in preference to the release error that failure causes. Decoding an outcome yields the output even when the command failed, because a failing command usually explains itself on standard error.

`Run` never deletes the Sandbox it created in either client. Disposal stays the caller's decision, matching the rule that a handle performs no lifecycle action implicitly.

The Go package import path is `github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient`. The TypeScript publication name is `@secondstack-ai/secondbox`; its repository manifest remains at the non-release version `0.0.0-development`. `npm run pack:sdk-typescript` performs a clean declaration/runtime build and dry-packs the exact public files without publishing.

## Same-host CLI

Interactive terminal presentation is automatic only for bounded commands.
`--output json` preserves the API's machine representation, `--output plain`
selects an unstyled human view, and `--color auto|always|never` controls color.
`NO_COLOR`, `TERM=dumb`, CI, and `--accessible` /
`SECONDBOX_ACCESSIBLE=1` are honored. The flags precede the command:

```sh
./dist/secondbox --output json sandboxes list | jq '.items'
./dist/secondbox --output plain --color never whoami
```

File content, logs, generic `operation` responses, terminal and
exec streams, and guest exit statuses remain raw. The normative classification
and script guarantees are in [CLI output and terminal presentation](cli-output-contract.md).

Build the standalone CLI and pass authentication explicitly:

```sh
go build -o ./dist/secondbox ./cmd/secondbox

./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref "$TENANT_REF" \
  --subject-ref "$SUBJECT_REF" \
  sandboxes list
```

### Credentials

Every command resolves the endpoint, token, tenant reference, and subject reference from the first source that supplies each value: the explicit flag, then the environment, then stored configuration. A value absent from all three is reported by the command that requires it, naming every source.

| Flag | Environment variable |
| --- | --- |
| `--url` | `SECONDBOX_URL` |
| `--token` | `SECONDBOX_TOKEN` |
| `--tenant-ref` | `SECONDBOX_TENANT_REF` |
| `--subject-ref` | `SECONDBOX_SUBJECT_REF` |

`SECONDBOX_TOKEN` is deliberately distinct from the `SECONDBOX_PLATFORM_TOKEN` that `secondboxd` reads. A shell configured to run the control plane does not thereby hand its deployment token to the CLI.

`login` verifies the credentials against the deployment before storing them, so a wrong token fails immediately with the server's problem detail and nothing is written:

```sh
./dist/secondbox login \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref "$TENANT_REF" \
  --subject-ref "$SUBJECT_REF"

./dist/secondbox sandboxes list
```

Configuration is stored at `$SECONDBOX_CONFIG` when that variable holds an absolute path, and otherwise at `config.json` under the `secondbox` directory of the user configuration directory, which honors `XDG_CONFIG_HOME`. The directory is created at mode `0700` and the file at mode `0600`, written under a temporary name and renamed into place so a concurrent reader never observes a partial document. Reads reject a symbolic link, a non-regular file, any group or other permission bit, an unknown JSON field, and trailing content.

`login` defaults each unspecified value to what the environment or existing configuration already resolves, so a shell that exports the four variables can persist them with a bare `login`. `whoami` reports the resolved endpoint, tenant reference, subject reference, and the origin of each, and reports only whether a token is present — it never prints the token. `logout` removes the stored configuration and succeeds when none exists.

These three commands are the only ones that do not accept an operation; every other command continues to work with fully explicit flags, and an explicit flag always wins over the environment and stored configuration.

Grouped aliases cover Profiles, RunnerPools, Runners, Sandboxes, Operations, Leases, streaming exec, terminal negotiation, files, Snapshot create/list/get/delete/restore, and ports. Their remaining arguments are thin transport values:

```sh
./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref "$TENANT_REF" \
  --subject-ref "$SUBJECT_REF" \
  sandboxes get \
  --path sandboxId=sbx_123
```

Use `operation <operationId>` to invoke any route in the generated transport table, including `executeSandboxCommand`. `--path`, `--query`, and `--header` accept repeatable `name=value` pairs; `--body` accepts a filename or `-`; `--content-type` selects the declared request media type. File bodies and responses stream between the selected file or standard input/output rather than being buffered by the CLI.

### Running one command

`exec` takes the Sandbox before any option and the guest command after `--`:

```sh
./dist/secondbox exec sbx_123 -- python3 -c 'print("hello from a microVM")'
```

The Sandbox operand comes first because everything after `--` belongs to the guest, including operands that look like CLI options. `--shell` selects a shell command instead of an argv command and requires exactly one operand:

```sh
./dist/secondbox exec sbx_123 --shell -- 'printf out; printf err >&2; exit 23'
```

The command retrieves the Sandbox and applies its current generation itself, and generates its own idempotency key, so neither is supplied by hand. `exec` needs no Lease; `--lease` and `--idempotency-key` remain available when a caller owns them already.

Guest standard output and standard error are decoded and written to the CLI's own two streams without being combined, and the guest's exit status becomes the CLI's exit status. The example above exits 23 and prints nothing of its own, exactly as a local command would. Every outcome that has no exit status — a command that never started, a deadline, an exhausted output bound, or an infrastructure failure — is instead described on standard error and exits 1, after any output the outcome carried has still been written.

`--deadline` defaults to one minute and `--max-output-bytes` to one mebibyte; the pinned Profile's execution policy bounds both. `--cwd` selects a workspace-relative directory and `--env name=value` is repeatable. `--json` writes the raw `ExecOutcome`, retaining base64 output for scripting, and still exits with the guest's status.

`--stdin` sends standard input to the command. The buffered route bounds it at one mebibyte, which is exactly what its base64 field admits, and an input that does not fit is refused rather than truncated; `exec stream` carries unbounded input.

### Naming a Sandbox

Every command that takes a Sandbox accepts either its opaque identifier or a name. Identifiers carry a fixed `sbx_` prefix, so the two are told apart without a speculative request.

A name is the reserved Metadata key `secondbox.dev/name`, which is unique per tenant and subject among Sandboxes that are not deleted. `listSandboxes` filters on it server-side, so a name resolves identically from any host and any SDK; nothing is cached locally. A deleted Sandbox releases its name, and because deleted Sandboxes keep their Metadata and remain listable, resolution skips them.

The service rejects a reserved name that could never resolve: one that is blank, one surrounded by whitespace, and one beginning with `sbx_`, which would shadow an identifier. Uniqueness itself is enforced by the database, and a collision is reported as a `state_conflict`. These rules apply to the Metadata key wherever it is written, through `createSandbox` and `updateSandboxMetadata` alike, not only through the CLI's `--name`.

### Running a one-off command

`run` creates a Sandbox from a Profile, waits for it to become ready, runs one command, and deletes the Sandbox:

```sh
./dist/secondbox run durable-coding -- python3 -c 'print("hello")'
```

`--name` reserves a name for later reference and `--keep` retains the Sandbox, reporting its identifier on standard error. `--metadata name=value` is repeatable and cannot restate the reserved name key. `--ready-timeout` bounds the wait for readiness and defaults to five minutes. Output handling, `--stdin`, and exit status match `exec` exactly, and the Sandbox is disposed of even when the command fails. Standard input is read before anything is created, so an oversized input leaves no Sandbox behind.

### Opening an interactive shell

```sh
./dist/secondbox shell my-box
```

For a throwaway session, `run --tty` creates the Sandbox, attaches the terminal, and deletes it when the terminal ends:

```sh
./dist/secondbox run durable-coding --tty
./dist/secondbox run durable-coding --tty -- /bin/bash
```

Disposal runs on every exit, including a dropped connection, because the Sandbox exists only to serve that session; `--keep` opts out and reports the identifier so `secondbox shell` can resume it. `--tty` cannot be combined with `--stdin`, `--json`, or `--shell`, which all describe a buffered command, and it accepts at most one operand, used as the terminal command. Both forms share one implementation, so the Lease, generation, and idempotency handling described below applies to each.

`shell` resolves the name, applies the Sandbox's current generation, acquires and renews a Lease for the session, and releases it on exit. It then opens the same real Terminal as `sandbox shell`: raw mode, local dimensions, `SIGWINCH` forwarding, byte-exact binary input and merged PTY output, and the original terminal mode restored on remote exit, cancellation, or transport failure.

Every value it supplies is an overridable default rather than a fixed choice, because injected arguments precede the caller's own. `--lease` or `--session` suppresses Lease acquisition, and `--command`, `--generation`, `--detachable`, `--rows`, `--columns`, and the rest behave as they do for `sandbox shell`. `sandbox shell` itself is unchanged and remains the fully explicit form.

Local operators can inspect a bounded tail or follow the configured control-plane JSON log without supplying API credentials:

```sh
./dist/secondbox \
  logs tail \
  --path /var/log/secondbox/control-plane.jsonl \
  --bytes 1048576

./dist/secondbox \
  logs follow \
  --path /var/log/secondbox/control-plane.jsonl \
  --bytes 1048576 \
  --poll-interval 250ms
```

Both commands require an absolute regular-file path and reject symbolic links, including a file replaced by a symbolic link while following. `--bytes` is mandatory, limited to 100 MiB, and bounds the initial output. `logs follow` then streams appended bytes and follows regular-file truncation or replacement until the process is interrupted.

The CLI also creates the same secret-avoiding support bundle as the deployment collector without invoking deployment scripts:

```sh
./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  diagnostics bundle \
  --output /secure/path/secondbox-support.tar.gz \
  --control-plane-log /var/log/secondbox/control-plane.jsonl \
  --max-log-bytes 1048576 \
  --max-probe-bytes 1048576 \
  --http-timeout 5s \
  --timing-window 15m
```

The bundle refuses to overwrite an existing path, is created with mode `0600`, and contains checksummed results for `healthz`, `readyz`, `metrics`, the aggregate timing summary, and the bounded log tail. Every HTTP and log bound is explicit. The CLI does not send the token to the three unauthenticated probes; it sends it only to the aggregate timing route and never writes it to the archive.

Use `timings sandbox --sandbox-id ... --limit ...`, `timings operation --operation-id ...`, and `timings summary --window ...` for human-readable lifecycle, boot-stage, Exec, and deployment percentiles. Each timing command requires the global URL, token, tenant, and subject options. See [Observability and diagnostics](observability-and-diagnostics.md) for the bounds and interpretation.

`exec stream` creates and attaches to a streaming session. It reads JSON objects from standard input, assigns their monotonically increasing client sequence, and writes the server's sequenced output and terminal frames as JSON Lines without combining stdout and stderr:

```sh
{
  printf '%s\n' \
    '{"type":"stdin","dataBase64":"aGVsbG8K","endOfInput":true}' \
    '{"type":"credit","bytes":4096}'
} | ./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref "$TENANT_REF" \
  --subject-ref "$SUBJECT_REF" \
  exec stream \
  --sandbox sbx_123 \
  --generation 4 \
  --idempotency-key req_stream_123 \
  --request ./streaming-exec-request.json
```

Input JSONL accepts only `stdin` with canonical `dataBase64` and an explicit `endOfInput` boolean, `credit` with positive `bytes`, and `cancel`. A `stdin` frame with `endOfInput: true` closes process standard input after its decoded bytes; an empty payload is valid only on that final frame, and later `stdin` frames are rejected. Output JSONL retains the public `output` or `outcome` schema and server sequence. `--create-only` prints the `ExecStreamSession` JSON without attaching. This is a protocol-oriented frame pump, not a raw shell or PTY interface.

Go callers can pass the session returned by `CreateExecStream` to `SandboxHandle.ConnectExecStream`, then use `SendInput`, `CloseInput` or `SendInputFrame`, `GrantOutput`, `Receive`, and `Cancel`. `CloseInput` emits an empty final stdin frame, while `SendInputFrame` can attach final bytes to the EOF signal. Closing a nonterminal Exec helper causes server-side guest cancellation without deleting the Sandbox.

Terminal creation returns a stable session ID and an authenticated `secondbox.terminal.v1` endpoint. Go callers pass that descriptor to `SandboxHandle.ConnectTerminal` and use `SendInput`, `Resize`, `GrantOutput`, `Receive`, and `Cancel`. TypeScript callers use `connectTerminal` with a `TerminalConnector`; the helper validates the Sandbox generation, endpoint, negotiated subprotocol, ordered server frames, and canonical base64 output. Both SDKs expose descriptor lookup and cancellation by the stable Terminal ID. The reconnect descriptor carries `nextClientSequence`, and both helpers begin writes at that retained position rather than restarting at zero. A detachable Terminal may reconnect within the pinned Profile's `terminalDetachSeconds` interval, while a non-detachable disconnect requests guest cancellation immediately. Only one attachment is active at a time, retained output replays from sequence zero after reconnect, and neither detach nor terminal cancellation stops or deletes the Sandbox.

The CLI opens a real interactive Terminal and restores the local terminal state on remote exit, cancellation, or transport failure:

```sh
./dist/secondbox \
  --url https://secondbox.example.com \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref "$TENANT_REF" \
  --subject-ref "$SUBJECT_REF" \
  sandbox shell \
  --sandbox sbx_123 \
  --generation 4 \
  --lease lea_123 \
  --idempotency-key req_shell_123 \
  --command /bin/bash \
  --detachable
```

When standard input and output are terminals, the command enters raw mode, uses the local dimensions, forwards `SIGWINCH` resizes, and restores the original mode before returning. Binary input and merged PTY output remain byte-exact; delivered bytes replenish the bounded output-credit window. `--session term_123` reconnects an existing detachable session instead of creating another guest process and cannot be combined with creation authority. For automation without a TTY, `--rows` and `--columns` select explicit dimensions. Audit export remains a database-operator command documented in [Observability and diagnostics](observability-and-diagnostics.md), rather than an HTTP or CLI resource.

## Remote-runner deployment

Clients still connect only to the control-plane HTTPS endpoint when compute runs on remote SecondBox runners. Do not expose runner listener addresses or credentials to SDK clients:

```sh
./dist/secondbox \
  --url https://secondbox.example.com \
  --token "$SECONDBOX_PLATFORM_TOKEN" \
  --tenant-ref "$TENANT_REF" \
  --subject-ref "$SUBJECT_REF" \
  profiles list
```

Runner certificate issuance, authenticated outbound Runner connectivity, and placement are operator concerns. SDK request shapes and Sandbox lifecycle calls are identical for same-host and remote-runner deployments.

## TypeScript lifecycle ownership

Application code creates, retains, and eventually deletes the durable Sandbox. A handle never performs lifecycle work on disconnect or Flue harness close:

```ts
import {
  SecondBox,
  SecondBoxClient,
} from "@secondstack-ai/secondbox";

const api = new SecondBox(
  new SecondBoxClient(endpoint, platformToken, fetch, tenantRef, subjectRef),
);

await api.validateProfile("durable-coding", signal);
const { handle } = await api.createSandbox({
  profile: "durable-coding",
  metadata: {},
  signal,
});
await handle.waitFor(["ready"], { deadlineMilliseconds: 300_000, signal });

// Reuse this handle across application requests and Flue initializations.

await handle.delete({
  signal,
});
```

Lifecycle methods generate one request key when absent and fence the handle's observed revision. A caller may supply a durable idempotency key or an explicit expected revision, but the SDK never refreshes and replays after a fence. Data-plane helpers bind the handle’s observed generation and optional Lease ID. Poll intervals, deadlines, and output limits remain explicit. The full operation matrix is in [Consumer operation matrix](../design/consumer-operation-matrix.md).

## Flue adapter

The adapter targets the exact public contract from [`@flue/runtime` 2.0.1](https://www.npmjs.com/package/@flue/runtime/v/2.0.1), upstream tag [`v2.0.1`](https://github.com/withastro/flue/tree/v2.0.1) at commit `a67f00955ac48c14d4b97ffb71962d24e39af84d`, package integrity `sha512-as+rrm8oHLuaLfpSReExwsuzOb1gC0sxQWgz3o+RvJUwHyGcQAcfBcn/R6j8logAq1j+ryfH1WsXf4J7Th9puQ==`, and Flue's [Sandbox Adapter API](https://flueframework.com/docs/reference/sandbox-api/). The package declares Flue 2.x as a peer and imports its real `createSandboxSessionEnv`, `SandboxApi`, `FileStat`, and `SandboxFactory`; there is no copied runtime or compatibility fallback.

```ts
import { createSecondBoxFlueAdapter } from "@secondstack-ai/secondbox/flue";

const flueSandbox = createSecondBoxFlueAdapter(handle, {
  defaultDeadlineMilliseconds: 60_000,
  maximumFileBytes: 4 * 1024 * 1024,
  maximumOutputBytes: 4 * 1024 * 1024,
});

const agent = await init({
  model,
  sandbox: flueSandbox,
});
```

Every Flue initialization receives a new in-memory session environment over the same application-owned handle. Files therefore remain in the durable SecondBox Workspace across separate harness initializations. Closing either harness does not stop or delete the Sandbox.

Compatibility tests run against Flue 2.0 itself and cover path normalization, cwd/environment/timeout forwarding, abort behavior, missing-parent creation and retry, filesystem option forwarding, and structural factory assignability. Updating the targeted Flue major requires a deliberate contract review; there is no fallback runtime mode.

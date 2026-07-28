# SDK, CLI, and Flue quick starts

The versioned OpenAPI contract generates Go, TypeScript, and Python transports. The handwritten Go and TypeScript layers add request decoding, structured errors, explicit operation polling, caller-owned Sandbox handles, and data-plane helpers without redefining wire models. The Go helpers own authenticated `secondbox.exec.v1` and `secondbox.terminal.v1` WebSocket attachments, ordered binary input, resize, credit, output/outcome reads, cancellation, and detach cleanup. The TypeScript helpers apply the same sequencing and terminal rules over injected authenticated connectors so browser and server runtimes can supply their distinct WebSocket credential mechanism without adding a transport dependency. Python remains generated-only because no current in-repository Python consumer needs additional behavior.

The Go package import path is `github.com/SecondStack-AI/SecondBox/sdk/go/secondboxclient`. The TypeScript publication name is `@secondstack-ai/secondbox`; its repository manifest remains at the non-release version `0.0.0-development`. `npm run pack:sdk-typescript` performs a clean declaration/runtime build and dry-packs the exact public files without publishing. A release process must replace the development version with the qualified release version and publish the package from the same source commit only after the release matrix passes.

## Same-host CLI

Build the standalone CLI and pass authentication explicitly:

```sh
go build -o ./dist/secondbox ./cmd/secondbox

./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_SERVICE_ACCOUNT_TOKEN" \
  auth check
```

Grouped aliases cover every operation in the current contract: projects, service accounts, keys, profiles, RunnerPools, Runners, Sandboxes, operations, leases, buffered and streaming exec negotiation, terminal negotiation, files, checkpoints, artifacts, and ports. Their remaining arguments are generated transport values:

```sh
./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_SERVICE_ACCOUNT_TOKEN" \
  sandboxes get \
  --path sandboxId=sbx_123

./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_SERVICE_ACCOUNT_TOKEN" \
  exec \
  --path sandboxId=sbx_123 \
  --header SecondBox-Generation=4 \
  --header Idempotency-Key=req_123 \
  --body ./exec-request.json
```

Use `operation <operationId>` to invoke any operation in the generated contract. `--path`, `--query`, and `--header` accept repeatable `name=value` pairs; `--body` accepts a filename or `-`; `--content-type` selects a declared request media type. File bodies and responses stream between the selected file or standard input/output rather than being buffered by the CLI.

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
  diagnostics bundle \
  --output /secure/path/secondbox-support.tar.gz \
  --control-plane-log /var/log/secondbox/control-plane.jsonl \
  --max-log-bytes 1048576 \
  --http-timeout 5s
```

The bundle refuses to overwrite an existing path, is created with mode `0600`, and contains checksummed results only for `healthz`, `readyz`, `metrics`, and the bounded log tail. Probe bodies are individually capped at 1 MiB. Diagnostic probes are intentionally unauthenticated and the CLI never sends `--token` to them.

`exec stream` creates and attaches to a streaming session. It reads JSON objects from standard input, assigns their monotonically increasing client sequence, and writes the server's sequenced output and terminal frames as JSON Lines without combining stdout and stderr:

```sh
{
  printf '%s\n' \
    '{"type":"stdin","dataBase64":"aGVsbG8K","endOfInput":true}' \
    '{"type":"credit","bytes":4096}'
} | ./dist/secondbox \
  --url http://127.0.0.1:8080 \
  --token "$SECONDBOX_SERVICE_ACCOUNT_TOKEN" \
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
  --token "$SECONDBOX_SERVICE_ACCOUNT_TOKEN" \
  sandbox shell \
  --sandbox sbx_123 \
  --generation 4 \
  --lease lea_123 \
  --idempotency-key req_shell_123 \
  --command /bin/bash \
  --detachable
```

When standard input and output are terminals, the command enters raw mode, uses the local dimensions, forwards `SIGWINCH` resizes, and restores the original mode before returning. Binary input and merged PTY output remain byte-exact; delivered bytes replenish the bounded output-credit window. `--session term_123` reconnects an existing detachable session instead of creating another guest process and cannot be combined with creation authority. For automation without a TTY, `--rows` and `--columns` select explicit dimensions. The generic `shell create`, `shell reconnect`, and `shell close` aliases remain available for direct generated-contract access. Audit export remains a database-operator command documented in [Observability and diagnostics](observability-and-diagnostics.md), rather than an HTTP or CLI resource.

## Remote-runner deployment

Clients still connect only to the control-plane HTTPS endpoint when compute runs on remote SecondBox runners. Do not expose runner listener addresses or credentials to SDK clients:

```sh
./dist/secondbox \
  --url https://secondbox.example.com \
  --token "$SECONDBOX_SERVICE_ACCOUNT_TOKEN" \
  profiles list
```

Runner enrollment, authenticated outbound runner connectivity, and runner placement are operator concerns. SDK request shapes and Sandbox lifecycle calls are identical for same-host and remote-runner deployments.

## TypeScript lifecycle ownership

Application code creates, retains, and eventually deletes the durable Sandbox. A handle never performs lifecycle work on disconnect or Flue harness close:

```ts
import {
  SandboxHandle,
  SecondBox,
} from "./sdk/typescript/client.ts";
import {
  SecondBoxClient,
  type Operation,
  type Sandbox,
} from "./sdk/typescript/secondbox-client.gen.ts";

const api = new SecondBox(
  new SecondBoxClient(endpoint, serviceAccountToken, fetch),
);

const created = await api.requestJSON<Sandbox | Operation>("createSandbox", {
  headers: { "Idempotency-Key": createRequestID },
  body: JSON.stringify({
    profile: profileName,
    metadata: {},
  }),
});

const sandbox =
  "state" in created && created.state === "pending"
    ? (await api.waitOperation(created.id, {
        intervalMilliseconds: 250,
        signal,
      })).sandbox!
    : created as Sandbox;

const handle = new SandboxHandle(api, sandbox);

// Reuse this handle across application requests and Flue initializations.

await handle.delete({
  idempotencyKey: deleteRequestID,
  ifMatch: currentETag,
  signal,
});
```

Lifecycle methods require caller-provided idempotency and `If-Match` values. Data-plane helpers bind the handle’s observed generation and optional lease ID. Poll intervals, deadlines, and output limits are explicit.

## Flue adapter

The adapter targets the public structural contract from [`@flue/runtime` 1.0.0-beta.9](https://www.npmjs.com/package/@flue/runtime/v/1.0.0-beta.9), package integrity `sha512-ksh0ZkTVyqQnGvU3OnbVX6luAJwe6tt8q7O0vn99b7Cx6XcPTXzY/YEkXrOtCHzV6ZwfSdO9ZfaWbhTD1tdQuQ==`, and Flue’s official [Sandbox Adapter API](https://flueframework.com/docs/api/sandbox-api/). SecondBox does not install the full runtime. Its Apache-2.0 compatibility module freezes only `SandboxApi`, `FileStat`, `SessionEnv`, the `SandboxFactory.createSessionEnv` shape, and the required wrapper behavior. [`flue-runtime-beta9-source.json`](../../sdk/typescript/flue-runtime-beta9-source.json) binds the module to the exact upstream tag, commit, source hashes, package integrity, and local adaptation hash.

```ts
import { createSecondBoxFlueAdapter } from "./sdk/typescript/flue.ts";

const flueSandbox = createSecondBoxFlueAdapter(handle, {
  defaultDeadlineMilliseconds: 60_000,
  maximumOutputBytes: 4 * 1024 * 1024,
});

const agent = await init({
  model,
  sandbox: flueSandbox,
});
```

Every Flue initialization receives a new in-memory session environment over the same application-owned handle. Files therefore remain in the durable SecondBox Workspace across separate harness initializations. Closing either harness does not stop or delete the Sandbox.

Compatibility tests preserve beta.9 path normalization, cwd/environment/timeout forwarding, pre/post abort behavior, missing-parent creation and single retry, filesystem option forwarding, and structural factory assignability. Updating the targeted Flue version requires a deliberate source review and refreshed hash evidence; there is no fallback runtime mode.

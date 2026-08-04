# API reference comparison

SecondBox borrows proven ergonomic shapes without inheriting another product's ownership or durability model. This comparison is pinned to primary source revisions so later upstream changes cannot silently alter SecondBox semantics.

## Source pins

| Project | Reviewed source |
| --- | --- |
| Gondolin | [`earendil-works/gondolin@29fa74d802112f29c720990aced26165e0d57d84`](https://github.com/earendil-works/gondolin/tree/29fa74d802112f29c720990aced26165e0d57d84), especially [`docs/sdk-vm.md`](https://github.com/earendil-works/gondolin/blob/29fa74d802112f29c720990aced26165e0d57d84/docs/sdk-vm.md), [`host/src/exec.ts`](https://github.com/earendil-works/gondolin/blob/29fa74d802112f29c720990aced26165e0d57d84/host/src/exec.ts), and [`docs/sdk-network.md`](https://github.com/earendil-works/gondolin/blob/29fa74d802112f29c720990aced26165e0d57d84/docs/sdk-network.md). |
| Microsandbox | [`superradcompany/microsandbox@b0e8eaad7a4d53fe2fbf1211a906a2d6bf3617cf`](https://github.com/superradcompany/microsandbox/tree/b0e8eaad7a4d53fe2fbf1211a906a2d6bf3617cf), especially the TypeScript [`sandbox.ts`](https://github.com/superradcompany/microsandbox/blob/b0e8eaad7a4d53fe2fbf1211a906a2d6bf3617cf/sdk/node-ts/src/sandbox.ts), [`exec.ts`](https://github.com/superradcompany/microsandbox/blob/b0e8eaad7a4d53fe2fbf1211a906a2d6bf3617cf/sdk/node-ts/src/exec.ts), and [`fs.ts`](https://github.com/superradcompany/microsandbox/blob/b0e8eaad7a4d53fe2fbf1211a906a2d6bf3617cf/sdk/node-ts/src/fs.ts). The old `zerocore-ai` URL redirects to this repository. |
| Kilntainers | [`Kiln-AI/Kilntainers@79e26f5b93ee664435150038b4ee31f01b4e7ec0`](https://github.com/Kiln-AI/Kilntainers/tree/79e26f5b93ee664435150038b4ee31f01b4e7ec0), especially [`backends/base.py`](https://github.com/Kiln-AI/Kilntainers/blob/79e26f5b93ee664435150038b4ee31f01b4e7ec0/src/kilntainers/backends/base.py) and [`server.py`](https://github.com/Kiln-AI/Kilntainers/blob/79e26f5b93ee664435150038b4ee31f01b4e7ec0/src/kilntainers/server.py). |
| Flue | [`@flue/runtime` 1.0.0-beta.9](https://www.npmjs.com/package/@flue/runtime/v/1.0.0-beta.9), repository tag [`v1.0.0-beta.9`](https://github.com/withastro/flue/tree/v1.0.0-beta.9) at commit `607d2613eb181a5e31c28a980847e101207d9fd3`, package integrity `sha512-ksh0ZkTVyqQnGvU3OnbVX6luAJwe6tt8q7O0vn99b7Cx6XcPTXzY/YEkXrOtCHzV6ZwfSdO9ZfaWbhTD1tdQuQ==`, and the official [Sandbox Adapter API](https://flueframework.com/docs/api/sandbox-api/). The reviewed structural subset and wrapper are frozen in `sdk/typescript/flue-runtime-beta9-compat.ts` with source and adaptation hashes in the adjacent JSON evidence. |

## Lifecycle

Gondolin exposes a local `VM` object with explicit create/start/close and a local session registry. Closing ends the VM. Microsandbox exposes local builder, start/get/list/remove, handle, stop, wait, snapshot, and attachment APIs. Kilntainers lazily creates one Sandbox for an MCP session and stops it with the session. These shapes validate the value of explicit handles and idempotent stop, but their local-process or session ownership is not SecondBox ownership.

SecondBox makes Sandbox durable server state, separates it from Instance compute, and never maps SDK object disposal, transport disconnect, or framework harness close to deletion. Start, drain, stop, relocation, Snapshot create/delete/restore, and Sandbox delete are durable asynchronous Operations. A stopped Sandbox resumes only on its current home Runner from the current local Workspace image; only an explicit operator relocation can change that home.

## Execution and cancellation

Gondolin distinguishes shell strings from exact argv, returns non-zero exits as results, exposes binary and text output, uses credit-based streaming, and supports PTY attach and resize. Its reviewed cancellation signal stops waiting but does not guarantee guest process termination.

Microsandbox exposes buffered exec, streaming handles, stdin sinks, signals, kill, wait, collect, PTY resize, and separate stdout/stderr or PTY output. Kilntainers accepts mutually exclusive shell `command` or argv `args`, stdin, working directory, timeout, and output limit, then returns stdout, stderr, exit code, and duration through one MCP tool.

SecondBox deliberately keeps shell and argv distinct, uses byte-credit backpressure, and returns non-zero guest exits as `exited`. Unlike the reviewed references, spawn failure, deadline, cancellation, output exhaustion, fencing, guest loss, Runner loss, and infrastructure failure remain distinct typed outcomes. Deadline and cancellation propagate to the guest process tree and require terminal acknowledgement; a local promise race is insufficient.

## Filesystem

Gondolin's VM filesystem includes binary/text and streaming reads and writes, stat, direct-child listing, mkdir, rename, and recursive/force delete across the visible guest filesystem. Microsandbox exposes binary/text and streaming access, metadata, list, mkdir, remove, copy, rename, and host-copy helpers. Kilntainers intentionally provides no separate filesystem API; agents use shell commands through `sandbox_exec`.

SecondBox exposes the subset required for ordinary SDKs and Flue: binary and UTF-8 read/write, stat, direct-child list, exists, mkdir, remove, and bounded streaming transfer. It deliberately limits paths to the Workspace rather than the full guest root, does not expose host-copy paths, and separates Artifact exchange from filesystem paths.

## PTY and ports

Gondolin provides PTY attach/resize and host-local ingress. Microsandbox provides streaming PTY control and network/port APIs. Kilntainers has no independent PTY or exposed-port resource in its single-tool contract.

SecondBox gives PTYs stable server session IDs, bounded detach and authenticated reconnect, explicit cancellation, and generation fencing. Exposed ports are authenticated expiring sessions for profile-approved guest ports. The control plane never discloses a runner address except to an authority holding the direct data-plane scope, for one admitted session, with the expected certificate SPKI SHA-256 pin.

## Flue adapter

The pinned Flue `SandboxApi` requires:

- `readFile`, `readFileBuffer`, and string-or-byte `writeFile`;
- `stat`, `readdir`, `exists`, `mkdir({recursive})`, and `rm({recursive, force})`;
- shell-string `exec` with cwd, environment, millisecond timeout, and optional `AbortSignal`.

The adapter accepts an already initialized SecondBox Sandbox handle and passes its Workspace root to the frozen beta.9 `createSandboxSessionEnv` adaptation. It rejects an unsupported mkdir or rm option before mutation, preserves real stat fidelity, maps SecondBox terminal outcomes into Flue conventions only at this boundary, and propagates timeout and abort to server-side cancellation. The full `@flue/runtime` graph is not a build or validation dependency; structural compatibility is enforced by TypeScript and wrapper conformance tests.

Flue owns no Sandbox lifecycle. Repeated harness initialization may reuse the same handle and retained files. Harness close never stops or deletes the Sandbox. Application code performs create, reuse, stop, and delete explicitly.

The integration contract runs the real TypeScript adapter through authenticated public HTTP handlers and the `SBXDP1` data plane. It proves missing-parent creation and retry, UTF-8 and binary persistence across separate session environments, stat, directory listing, negative existence, mkdir and remove options, and shell execution with cwd, environment, deadline, output bound, and non-zero exit fidelity.

See [API conventions](api-conventions.md) and [Domain and lifecycle](domain-lifecycle.md).

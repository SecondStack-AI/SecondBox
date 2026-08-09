# Consumer operation matrix

The public Go package at `sdk/go/secondboxclient` and the TypeScript package
`@secondstack-ai/secondbox` own the generic mechanics below. A consumer maps its
own identities and orchestration onto these operations; it does not construct
OpenAPI operation names, generation headers, ETags, polling loops, multipart
bodies, or response decoders.

| Consumer need | Go | TypeScript | Required behavior |
| --- | --- | --- | --- |
| Validate a Profile | `Client.ValidateProfile` | `SecondBox.validateProfile` | Return only an enabled named Profile; preserve immutable revision identity. |
| Create/adopt a Sandbox | `CreateSandbox`, `AdoptSandbox` | `createSandbox`, `adoptSandbox` | Return a caller-owned handle; never delete implicitly. |
| List Sandboxes | `ListSandboxes` | `listSandboxes` | Typed bounded pagination and deterministic Metadata containment filters. |
| Replace Metadata | `SandboxHandle.UpdateMetadata` | `SandboxHandle.updateMetadata` | Fence against the handle's observed resource revision; never refresh and replay. |
| Lifecycle | `Start`, `Drain`, `Stop`, `Relocate`, `Restore`, `Delete` | matching handle methods | Generate one idempotency key when absent and use the observed revision unless an explicit expected revision is supplied. |
| Wait/poll | `Wait`, `WaitFor`, `WaitOperation` | matching methods | Require caller cancellation/deadline and bounded individual polls; surface terminal failures. |
| Buffered execution | `Execute`, `DecodeExecOutcome`, `Run` | `exec`, `decodeExecOutcome`, `run` | Fence the observed generation, bound output/deadline, preserve terminal output and caller-owned Sandbox lifetime. |
| Streaming execution | `CreateExecStream`, `ConnectExecStream` | matching methods plus an injected connector | Preserve sequence, flow-control, EOF, cancellation and terminal outcome. Closing cancels process work, not the Sandbox. |
| Filesystem | `ReadFile`, `WriteFile`, `StatFile`, `ListDirectory`, `FileExists`, `CreateDirectory`, `RemovePath` | matching handle methods | Workspace-relative only, generation/Lease fenced, bounded reads and digest-checked writes. |
| Snapshots | `CreateSnapshot`, `ListSnapshots`, `GetSnapshot`, `DeleteSnapshot`, `Restore` | matching methods | Explicit optimistic revision and idempotency; no implicit restore or deletion. |
| Lease takeover | `TakeoverLease`, `KeepLease` | `takeoverLease`, `keepLease` | Explicit generation fence; a keeper releases the Lease but never the Sandbox. |
| Named Ports | `CreatePortSession`, `GetPortSession`, `ClosePortSession`, `ConnectPortTunnel` | matching methods | Consume one credential through its declared proxied or direct transport only. |
| Terminals | `CreateTerminal`, `GetTerminal`, `ConnectTerminal`, `CancelTerminal` | matching methods | Authenticated, generation-fenced, sequenced attach/reconnect without lifecycle ownership. |
| Node transports | not applicable | `@secondstack-ai/secondbox/node` | Authenticated WebSockets and TLS 1.3 direct-port dialing with the admitted SPKI pin. |
| Flue 2.0 | language-neutral consumer mapping | `@secondstack-ai/secondbox/flue` | Exact `@flue/runtime` 2.x `SandboxFactory`/`SandboxApi` contract over an existing handle. |

Admissions, Agent identity, Chat threads, external-effect receipts, Flue shard
ownership, and consumer database mappings are intentionally absent. Those are
consumer semantics rather than SecondBox resource or transport mechanics.

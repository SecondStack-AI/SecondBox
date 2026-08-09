# `@secondstack-ai/secondbox`

This package contains generated SecondBox v1 operation metadata and wire types, plus handwritten HTTP mechanics and helpers for durable Sandbox lifecycle, polling, execution, filesystem access, terminal negotiation, and the Flue adapter.

Applications own Sandbox lifetime. Closing an SDK object, WebSocket, or Flue harness never stops or deletes the Sandbox.

```ts
import {
  SecondBox,
  SecondBoxClient,
} from "@secondstack-ai/secondbox";

const token = process.env.SECONDBOX_API_TOKEN;
if (token === undefined) {
  throw new Error("SECONDBOX_API_TOKEN is required");
}
const transport = new SecondBoxClient(
  "https://secondbox.example",
  token,
  fetch,
);
const secondbox = new SecondBox(transport);
```

Import the public wire types from `@secondstack-ai/secondbox`, the Flue 2.x adapter from `@secondstack-ai/secondbox/flue`, and Node's authenticated WebSocket plus SPKI-pinned direct-port transports from `@secondstack-ai/secondbox/node`.

`SandboxHandle.connectExecStream` accepts an application-supplied authenticated connector. The returned helper validates the Sandbox generation and WebSocket subprotocol, sequences stdin, explicit EOF, credit, and cancellation frames, and rejects input after EOF or any operation after the terminal outcome. The connector owns runtime-specific credential attachment because browser and server WebSocket implementations expose different authentication surfaces.

High-level methods cover Profiles, RunnerPools, Sandbox creation/adoption/listing, Metadata, lifecycle and waiting, buffered/streaming execution, files, Snapshots, Leases, Ports, and terminals. They generate request keys when absent and translate observed resource revisions into optimistic-concurrency headers without refreshing and replaying fenced work.

The canonical API contract, runnable examples, and deployment guidance live in the [SecondBox repository](https://github.com/SecondStack-AI/SecondBox).

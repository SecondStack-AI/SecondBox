# `@secondstack-ai/secondbox`

This package contains the generated SecondBox v1 HTTP transport and small handwritten helpers for durable Sandbox lifecycle, polling, execution, filesystem access, terminal negotiation, and the Flue adapter.

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

Import generated wire types from `@secondstack-ai/secondbox/generated` and the Flue adapter from `@secondstack-ai/secondbox/flue`.

`SandboxHandle.connectExecStream` accepts an application-supplied authenticated connector. The returned helper validates the Sandbox generation and WebSocket subprotocol, sequences stdin, explicit EOF, credit, and cancellation frames, and rejects input after EOF or any operation after the terminal outcome. The connector owns runtime-specific credential attachment because browser and server WebSocket implementations expose different authentication surfaces.

The canonical API contract and deployment guidance live in the [SecondBox repository](https://github.com/SecondStack-AI/SecondBox).

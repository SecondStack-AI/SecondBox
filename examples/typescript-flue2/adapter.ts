import type { SandboxFactory } from "@flue/runtime";
import {
  createSecondBoxFlueAdapter,
} from "@secondstack-ai/secondbox/flue";
import type { SandboxHandle } from "@secondstack-ai/secondbox";

// Application code resolves and retains the durable Sandbox. The Flue factory
// maps only generic command and filesystem behavior and owns no lifecycle.
export function durableCodingFlueSandbox(handle: SandboxHandle): SandboxFactory {
  return createSecondBoxFlueAdapter(handle, {
    defaultDeadlineMilliseconds: 60_000,
    maximumOutputBytes: 1 << 20,
  });
}

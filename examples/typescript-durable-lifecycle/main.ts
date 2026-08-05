import {
  SecondBox,
  SecondBoxClient,
} from "@secondstack-ai/secondbox";

const api = new SecondBox(new SecondBoxClient(
  required("SECONDBOX_URL"),
  required("SECONDBOX_TOKEN"),
  fetch,
  required("SECONDBOX_TENANT_REF"),
  required("SECONDBOX_SUBJECT_REF"),
));

await api.validateProfile("durable-coding");
const { handle } = await api.createSandbox({
  profile: "durable-coding",
  metadata: { example: "typescript" },
});
await handle.waitFor(["ready"], { deadlineMilliseconds: 10 * 60_000 });
const result = await handle.exec({
  mode: "argv",
  executable: "printf",
  arguments: ["durable"],
}, {
  environment: {},
  deadlineMilliseconds: 30_000,
  maximumOutputBytes: 1 << 20,
});
console.log(handle.snapshot.id, new TextDecoder().decode(result.kind === "exited" ? result.stdout : new Uint8Array()));
await handle.stop({});
// The stopped Sandbox and Workspace remain durable until explicit deletion.

function required(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

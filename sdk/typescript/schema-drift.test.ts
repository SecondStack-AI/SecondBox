import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

/** The generated interface names and properties must mirror the contract. */
const here = dirname(fileURLToPath(import.meta.url));
const generatedTransportSource = readFileSync(join(here, "transport.generated.ts"), "utf8");
const transportSource =
  generatedTransportSource + readFileSync(join(here, "transport-runtime.ts"), "utf8");
const document = JSON.parse(
  readFileSync(
    join(here, "..", "..", "contracts", "openapi", "v1", "secondbox.openapi.json"),
    "utf8",
  ),
) as {
  components: { schemas: Record<string, { properties?: Record<string, unknown> }> };
};

/**
 * Every generated interface that shares a name with a canonical schema.
 */
const SCHEMA_BACKED_INTERFACES = [...generatedTransportSource.matchAll(/^export interface ([A-Za-z0-9_]+) \{/gm)]
  .map((match) => match[1] as string)
  .filter((name) => name in document.components.schemas)
  .sort();

function schemaProperties(name: string): string[] {
  const schema = document.components.schemas[name];
  assert(schema, `canonical contract has no schema ${name}`);
  assert(schema.properties, `schema ${name} declares no properties`);
  return Object.keys(schema.properties).sort();
}

function interfaceProperties(name: string): string[] {
  const declaration = new RegExp(
    `export interface ${name} \\{([\\s\\S]*?)\\n\\}`,
    "m",
  ).exec(generatedTransportSource);
  assert(declaration, `transport.ts declares no interface ${name}`);
  const names: string[] = [];
  let depth = 0;
  for (const line of (declaration[1] ?? "").split("\n")) {
    // Only the interface's own properties count. A nested object literal
    // declares its own, and those belong to that shape rather than this one.
    const property = /^\s*readonly\s+([A-Za-z0-9_]+)\??\s*:/.exec(line);
    if (depth === 0 && property?.[1]) names.push(property[1]);
    depth += (line.match(/\{/g) ?? []).length - (line.match(/\}/g) ?? []).length;
  }
  return names.sort();
}

for (const name of SCHEMA_BACKED_INTERFACES) {
  test(`TypeScript ${name} mirrors the canonical schema`, () => {
    assert.deepEqual(
      interfaceProperties(name),
      schemaProperties(name),
      `${name} generated properties differ from the canonical schema.`,
    );
  });
}

/**
 * The published surface, as a committed snapshot.
 *
 * Adding a required property to an interface is a breaking change for every
 * consumer that constructs it, and this SDK reaches consumers through a file:
 * dependency where nothing else announces that. Updating this snapshot is the
 * step that makes such a change visible in review rather than incidental.
 */
const surfacePath = join(here, "public-surface.json");

function publicSurface(): Record<string, { required: string[]; optional: string[] }> {
  const surface: Record<string, { required: string[]; optional: string[] }> = {};
  for (const match of transportSource.matchAll(/^export interface ([A-Za-z0-9_]+) \{/gm)) {
    const name = match[1] as string;
    const declaration = new RegExp(
      `export interface ${name} \\{([\\s\\S]*?)\\n\\}`,
      "m",
    ).exec(transportSource);
    if (!declaration) continue;
    const required: string[] = [];
    const optional: string[] = [];
    let depth = 0;
    for (const line of (declaration[1] ?? "").split("\n")) {
      const property = /^\s*readonly\s+([A-Za-z0-9_]+)(\??)\s*:/.exec(line);
      if (depth === 0 && property?.[1]) {
        (property[2] === "?" ? optional : required).push(property[1]);
      }
      depth += (line.match(/\{/g) ?? []).length - (line.match(/\}/g) ?? []).length;
    }
    surface[name] = { required: required.sort(), optional: optional.sort() };
  }
  return surface;
}

test("the published TypeScript surface matches its committed snapshot", () => {
  const actual = publicSurface();
  const expected = JSON.parse(readFileSync(surfacePath, "utf8")) as typeof actual;
  const breaking: string[] = [];
  for (const [name, shape] of Object.entries(actual)) {
    const before = expected[name];
    if (!before) continue;
    for (const property of shape.required) {
      if (!before.required.includes(property)) {
        breaking.push(`${name}.${property} became required`);
      }
    }
    for (const property of before.required) {
      if (!shape.required.includes(property) && !shape.optional.includes(property)) {
        breaking.push(`${name}.${property} was removed`);
      }
    }
  }
  assert.deepEqual(
    actual,
    expected,
    breaking.length > 0
      ? `Breaking change to the published surface:\n  ${breaking.join("\n  ")}\n` +
          `Every consumer constructing these types must be updated. ` +
          `Regenerate sdk/typescript/public-surface.json once that is accounted for.`
      : `The published surface changed. Regenerate sdk/typescript/public-surface.json.`,
  );
});

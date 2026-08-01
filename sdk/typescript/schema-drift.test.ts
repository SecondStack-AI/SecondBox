import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

/**
 * The TypeScript wire types are hand-maintained against the canonical OpenAPI
 * document, and TypeScript erases them at runtime, so nothing else can observe
 * a property that the document declares and this SDK never carries. Go has the
 * compiler as a partial backstop; this SDK has none, which is why the
 * PortSession `transport` addition broke a downstream consumer silently.
 *
 * The interface source is parsed as text rather than compiled, because the
 * property names are exactly what a client depends on and the file's shape is
 * ours to keep simple.
 */
const here = dirname(fileURLToPath(import.meta.url));
const transportSource = readFileSync(join(here, "transport.ts"), "utf8");
const document = JSON.parse(
  readFileSync(
    join(here, "..", "..", "contracts", "openapi", "v1", "secondbox.openapi.json"),
    "utf8",
  ),
) as {
  components: { schemas: Record<string, { properties?: Record<string, unknown> }> };
};

/**
 * Every interface this SDK declares that shares a name with a canonical schema.
 *
 * Derived rather than listed, so a newly added interface is checked without
 * anyone remembering to register it, and an interface this SDK deliberately does
 * not model is simply absent rather than a false failure.
 */
const SCHEMA_BACKED_INTERFACES = [...transportSource.matchAll(/^export interface ([A-Za-z0-9_]+) \{/gm)]
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
  ).exec(transportSource);
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

/**
 * This SDK deliberately models a narrower surface than the full API, so an
 * omitted property is a product decision rather than a defect. A property this
 * SDK declares that the document does not is always a defect: it reads as
 * undefined at runtime with no type error, which is precisely the failure a
 * hand-maintained mirror invites.
 */
for (const name of SCHEMA_BACKED_INTERFACES) {
  test(`TypeScript ${name} declares no property the schema lacks`, () => {
    const declared = schemaProperties(name);
    const phantom = interfaceProperties(name).filter(
      (property) => !declared.includes(property),
    );
    assert.deepEqual(
      phantom,
      [],
      `${name} declares ${JSON.stringify(phantom)}, which the canonical schema does not. ` +
        `A client reading it receives undefined with no type error.`,
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

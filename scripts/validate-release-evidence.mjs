#!/usr/bin/env node

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";

if (process.argv.length !== 3) {
  console.error("Usage: scripts/validate-release-evidence.mjs RELEASE_EVIDENCE.json");
  process.exit(2);
}

const scriptDirectory = path.dirname(fileURLToPath(import.meta.url));
const repositoryRoot = path.resolve(scriptDirectory, "..");
const schemaPath = path.join(repositoryRoot, "release", "evidence-schema.json");
const evidencePath = path.resolve(process.argv[2]);

function readJSON(documentPath, label) {
  let contents;
  try {
    contents = fs.readFileSync(documentPath, "utf8");
  } catch (error) {
    console.error(`SecondBox release evidence ${label} could not be read: ${error.message}`);
    process.exit(1);
  }
  try {
    return JSON.parse(contents);
  } catch (error) {
    console.error(`SecondBox release evidence ${label} is not valid JSON: ${error.message}`);
    process.exit(1);
  }
}

const schema = readJSON(schemaPath, "schema");
const evidence = readJSON(evidencePath, "document");
const ajv = new Ajv2020({
  allErrors: true,
  strict: true,
  formats: {
    "date-time": {
      type: "string",
      validate: (value) =>
        /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$/.test(value) &&
        !Number.isNaN(Date.parse(value)),
    },
  },
});
const validate = ajv.compile(schema);

if (!validate(evidence)) {
  const details = validate.errors
    .map((error) => `${error.instancePath || "/"} ${error.message}`)
    .join("; ");
  console.error(`SecondBox release evidence schema validation failed: ${details}`);
  process.exit(1);
}

console.log("SecondBox release evidence schema validation passed");

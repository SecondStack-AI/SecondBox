import {
  readFile,
  readdir,
  rm,
  writeFile,
} from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const packageDirectory = fileURLToPath(new URL("./", import.meta.url));
const distributionDirectory = fileURLToPath(new URL("./dist/", import.meta.url));

await rm(distributionDirectory, { force: true, recursive: true });

const result = spawnSync(
  "tsc",
  ["-p", "tsconfig.build.json"],
  {
    cwd: packageDirectory,
    encoding: "utf8",
    shell: process.platform === "win32",
    stdio: "inherit",
  },
);
if (result.error !== undefined) {
  throw result.error;
}
if (result.status !== 0) {
  throw new Error(`SecondBox TypeScript SDK build failed with status ${String(result.status)}`);
}

for (const entry of await readdir(distributionDirectory, { withFileTypes: true })) {
  if (!entry.isFile() || !entry.name.endsWith(".d.ts")) {
    continue;
  }
  const declarationPath = new URL(`./dist/${entry.name}`, import.meta.url);
  const declaration = await readFile(declarationPath, "utf8");
  const rewritten = declaration.replaceAll(
    /(from\s+["'][^"']+)\.ts(["'])/g,
    "$1.js$2",
  );
  await writeFile(declarationPath, rewritten, "utf8");
}

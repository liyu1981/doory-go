// Prints the build targets declared in package.json ("scripts") with the
// descriptions kept in "targets-help". The Makefile-style `help` target.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const pkg = JSON.parse(readFileSync(join(dirname(fileURLToPath(import.meta.url)), "..", "package.json"), "utf8"));
const help = pkg["targets-help"] ?? {};
const width = Math.max(...Object.keys(pkg.scripts).map((s) => s.length));
for (const name of Object.keys(pkg.scripts)) {
  console.log(`  ${name.padEnd(width)}  ${help[name] ?? ""}`.trimEnd());
}

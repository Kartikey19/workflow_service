import { readFile } from "node:fs/promises";
import ts from "typescript";

// Load a dependency-free TypeScript module's actual implementation in Node.
export async function loadModule(name) {
  const source = await readFile(
    new URL(`../src/${name}.ts`, import.meta.url),
    "utf8",
  );
  const compiled = ts.transpileModule(source, {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  }).outputText;
  return import(
    `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`
  );
}

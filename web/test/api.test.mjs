import { readFile } from "node:fs/promises";
import { test } from "node:test";
import assert from "node:assert/strict";
import ts from "typescript";

// Exercise the actual helper with simulated HTTP responses; no browser or
// network is needed to reproduce cancellation while a JSON body is reading.
const source = await readFile(
  new URL("../src/api.ts", import.meta.url),
  "utf8",
);
const compiled = ts.transpileModule(source, {
  compilerOptions: {
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.ESNext,
  },
}).outputText;
const { api, APIError } = await import(
  `data:text/javascript;base64,${Buffer.from(compiled).toString("base64")}`
);

test("an aborted JSON body remains cancellation, never successful resource data", async (t) => {
  const controller = new AbortController();
  const cancellation = new DOMException("Request cancelled", "AbortError");
  t.mock.method(globalThis, "fetch", async () => ({
    status: 200,
    ok: true,
    json: async () => {
      controller.abort();
      throw cancellation;
    },
  }));
  await assert.rejects(
    api("/runs/test/events", "GET", undefined, controller.signal),
    (error) => error === cancellation,
  );
});

test("unreadable success is an error, and unreadable failure keeps its HTTP status", async (t) => {
  for (const status of [200, 401]) {
    const mock = t.mock.method(globalThis, "fetch", async () => ({
      status,
      ok: status === 200,
      json: async () => {
        throw new SyntaxError("Invalid JSON");
      },
    }));
    await assert.rejects(api("/runs/test/events"), (error) => {
      assert.match(error.message, /unreadable response/);
      if (status === 401)
        assert.ok(error instanceof APIError && error.status === 401);
      return true;
    });
    mock.mock.restore();
  }
});

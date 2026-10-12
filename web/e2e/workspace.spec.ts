import { test, expect, type Page } from "@playwright/test";
import { parse, stringify } from "yaml";
// Set UI_TEST_SHOTS to a directory to keep review screenshots of key screens.
const shot = async (page: Page, name: string) => {
  if (process.env.UI_TEST_SHOTS)
    await page.screenshot({
      path: `${process.env.UI_TEST_SHOTS}/${name}.png`,
      fullPage: true,
    });
};
// Steps are added from the step catalog: open it, search, pick the first match.
const addStep = async (page: Page, search: string) => {
  const box = page.getByLabel("Search steps and apps");
  await page.getByRole("button", { name: "Add step", exact: true }).click();
  await box.fill(search);
  await page.getByRole("dialog").getByRole("option").first().click();
  await expect(box).toHaveCount(0);
};
const signInOwner = async (page: Page, email = "browserco@example.com") => {
  await page.goto("/ui/");
  await page.getByLabel("Email", { exact: true }).fill(email);
  await page
    .getByLabel("Password", { exact: true })
    .fill("owner-long-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Workflows", exact: true }),
  ).toBeVisible();
};
const openStart = async (page: Page) => {
  const row = page.getByRole("button", { name: /^Start/ });
  if ((await row.getAttribute("aria-expanded")) !== "true") await row.click();
};
test("edit typed start variables without losing schema rules, drafts or history", async ({
  page,
}) => {
  await signInOwner(page);
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page.getByLabel("Workflow name", { exact: true }).fill("typed-inputs");
  await openStart(page);
  await page.getByRole("button", { name: "Add variable", exact: true }).click();
  const first = page.getByRole("group", { name: "Variable 1", exact: true });
  const second = page.getByRole("group", { name: "Variable 2", exact: true });
  await expect(first.getByLabel("Name", { exact: true })).toBeFocused();
  await first.getByLabel("Name", { exact: true }).fill("amount");
  await first.getByLabel("Type", { exact: true }).selectOption("number");
  await expect(first.getByRole("option")).toHaveCount(7);
  await page.getByRole("button", { name: "Add variable", exact: true }).click();
  await second.getByLabel("Name", { exact: true }).fill("reason");
  await second.getByLabel("Required", { exact: true }).uncheck();
  await second.getByLabel("Name", { exact: true }).fill("amount");
  await expect(second.getByRole("alert")).toContainText(
    "already called amount",
  );
  await second.getByLabel("Name", { exact: true }).press("Tab");
  await expect(second.getByLabel("Name", { exact: true })).toHaveValue(
    "reason",
  );
  await second.getByLabel("Name", { exact: true }).fill("bad name");
  await expect(second.getByRole("alert")).toContainText("has not been changed");
  await second.getByLabel("Name", { exact: true }).press("Tab");
  await expect(second.getByLabel("Name", { exact: true })).toHaveValue(
    "reason",
  );
  await second.getByLabel("Name", { exact: true }).fill("notes");
  await expect(second.getByRole("alert")).toHaveCount(0);
  await first
    .getByRole("button", { name: "Remove variable amount", exact: true })
    .click();
  await page.getByRole("button", { name: "Undo", exact: true }).click();
  await openStart(page);
  await expect(first.getByLabel("Name", { exact: true })).toHaveValue("amount");
  await page.getByRole("button", { name: "Redo", exact: true }).click();
  await openStart(page);
  await expect(first.getByLabel("Name", { exact: true })).toHaveValue("notes");
  await page.getByRole("button", { name: "Undo", exact: true }).click();
  await page.reload();
  await openStart(page);
  await expect(first.getByLabel("Name", { exact: true })).toHaveValue("amount");
  await expect(first.getByLabel("Type", { exact: true })).toHaveValue("number");
  await expect(second.getByLabel("Name", { exact: true })).toHaveValue("notes");
  await expect(
    second.getByLabel("Required", { exact: true }),
  ).not.toBeChecked();
  // Advanced schema rules imported through YAML survive all row edits.
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  const yaml = page.getByLabel("Workflow YAML", { exact: true });
  const definition = parse(await yaml.inputValue());
  definition.inputs.amount = {
    ...definition.inputs.amount,
    default: 5,
    minimum: 1,
    maximum: 99,
  };
  definition.inputs.notes = {
    ...definition.inputs.notes,
    default: "one",
    enum: ["one", "two"],
    pattern: "^[a-z]+$",
  };
  await yaml.fill(stringify(definition));
  await page.getByRole("button", { name: "Canvas", exact: true }).click();
  await openStart(page);
  await first.getByLabel("Name", { exact: true }).fill("total");
  await first.getByLabel("Type", { exact: true }).selectOption("integer");
  await first.getByLabel("Required", { exact: true }).uncheck();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  const changed = parse(await yaml.inputValue());
  expect(changed.inputs).toEqual({
    total: {
      type: "integer",
      required: false,
      default: 5,
      minimum: 1,
      maximum: 99,
    },
    notes: definition.inputs.notes,
  });
  await page.getByRole("button", { name: "Canvas", exact: true }).click();
  await openStart(page);
  await shot(page, "12-typed-start-inputs");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(first.getByLabel("Type", { exact: true })).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.setViewportSize({ width: 1440, height: 900 });
  await addStep(page, "Transform");
  await page
    .getByLabel("Settings", { exact: true })
    .fill("value:\n  total: ${input.total}\n  notes: ${input.notes}");
  await expect(page.locator(".check-state")).toContainText("Valid workflow");
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(page.getByLabel("Total", { exact: true })).toHaveValue("5");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await page.locator(".node-attempt summary").first().click();
  await expect(page.locator(".node-attempt").first()).toContainText(
    '"total": 5',
  );
});
test("recover a draft after refresh, undo canvas and YAML edits, and clear it after publication", async ({
  page,
}) => {
  await signInOwner(page);
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("draft-recovery");
  await addStep(page, "AI step");
  await page.getByLabel("Model", { exact: true }).fill("fake-model");
  await page
    .getByLabel("Agent prompt", { exact: true })
    .fill("Recover this prompt, including pending input.");
  // Refresh before the structured-field debounce finishes.
  await page.reload();
  await expect(
    page.getByText("Recovered your unpublished draft.", { exact: false }),
  ).toBeVisible();
  await expect(page.getByLabel("Workflow name", { exact: true })).toHaveValue(
    "draft-recovery",
  );
  await page.getByRole("button", { name: /^(?:1 )?agent_1(?: |$)/ }).click();
  await expect(page.getByLabel("Agent prompt", { exact: true })).toHaveValue(
    "Recover this prompt, including pending input.",
  );
  await shot(page, "10-recovered-draft");
  await page.getByRole("button", { name: "Delete step", exact: true }).click();
  await page.getByRole("button", { name: "Undo", exact: true }).click();
  await page.getByRole("button", { name: /^(?:1 )?agent_1(?: |$)/ }).click();
  await expect(page.getByLabel("Agent prompt", { exact: true })).toHaveValue(
    "Recover this prompt, including pending input.",
  );
  await page.getByRole("button", { name: "Redo", exact: true }).click();
  await expect(
    page.getByRole("button", { name: /^(?:1 )?agent_1(?: |$)/ }),
  ).toHaveCount(0);
  await page.locator("body").press("Control+z");
  await expect(
    page.getByRole("button", { name: /^(?:1 )?agent_1(?: |$)/ }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Add provider account", exact: true })
    .click();
  const setup = page.getByRole("dialog", {
    name: "Add provider account",
    exact: true,
  });
  await setup
    .getByLabel("API key", { exact: true })
    .fill("draft-setup-key-must-not-be-saved");
  await setup.getByRole("button", { name: "Close setup" }).click();
  expect(
    await page.evaluate(() =>
      Object.keys(localStorage)
        .filter((key) => key.startsWith("weft:drafts:"))
        .some((key) =>
          localStorage
            .getItem(key)
            ?.includes("draft-setup-key-must-not-be-saved"),
        ),
    ),
  ).toBe(false);
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  const yaml = page.getByLabel("Workflow YAML", { exact: true });
  const saved = await yaml.inputValue();
  await yaml.fill("nodes: [invalid but recoverable");
  await page.reload();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  await expect(yaml).toHaveValue("nodes: [invalid but recoverable");
  await yaml.fill(saved);
  await page.getByRole("button", { name: "Canvas", exact: true }).click();
  await page.getByRole("button", { name: "Undo", exact: true }).click();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  await expect(yaml).toHaveValue("nodes: [invalid but recoverable");
  await page.getByRole("button", { name: "Redo", exact: true }).click();
  await expect(yaml).toHaveValue(saved);
  await page
    .getByRole("button", { name: "All workflows", exact: true })
    .click();
  await page.getByRole("button", { name: "Resume draft", exact: true }).click();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  const runnable =
    "apiVersion: workflow/v1\nname: draft-recovery\nnodes:\n  - id: done\n    type: transform\n    config: {value: {result: restored}}\n";
  await yaml.fill(runnable);
  await expect(
    page.getByText("Draft saved on this browser", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Valid workflow", { exact: true })).toBeVisible();
  await shot(page, "10-draft-recovery");
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Run draft-recovery", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("No unpublished changes", { exact: true }),
  ).toBeVisible();
  await page.reload();
  await expect(
    page.getByRole("button", { name: "Resume draft", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("button", { name: /^draft-recovery / }).click();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  await expect(yaml).toHaveValue(runnable);
});

test("a newer published version does not replace a local draft, and storage failures are visible", async ({
  page,
}) => {
  await signInOwner(page);
  const definition = (value: string) =>
    `apiVersion: workflow/v1\nname: draft-conflict\nnodes:\n  - id: done\n    type: transform\n    config: {value: {result: ${value}}}\n`;
  expect(
    (
      await page.request.post("/be/v1/workflows", {
        data: { yaml: definition("first") },
      })
    ).ok(),
  ).toBe(true);
  await page.reload();
  await page.getByRole("button", { name: /^draft-conflict / }).click();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  const yaml = page.getByLabel("Workflow YAML", { exact: true });
  await yaml.fill(definition("local"));
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByLabel("Email", { exact: true })).toBeVisible();
  await signInOwner(page, "freshsetup@example.com");
  await expect(
    page.getByRole("button", { name: "New workflow", exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("Workflow name", { exact: true })).toHaveCount(
    0,
  );
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByLabel("Email", { exact: true })).toBeVisible();
  await signInOwner(page);
  expect(
    (
      await page.request.post("/be/v1/workflows", {
        data: { yaml: definition("remote") },
      })
    ).ok(),
  ).toBe(true);
  await page.reload();
  await expect(
    page.getByText("A newer version was published", { exact: false }),
  ).toBeVisible();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  await expect(yaml).toHaveValue(definition("local"));
  await page
    .getByRole("button", { name: "Revert changes", exact: true })
    .click();
  await expect(yaml).toHaveValue(definition("remote"));
  await page.getByRole("button", { name: "Undo", exact: true }).click();
  await expect(yaml).toHaveValue(definition("local"));
  await page.addInitScript(() => {
    const set = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith("weft:drafts:"))
        throw new DOMException("Full", "QuotaExceededError");
      return set.call(this, key, value);
    };
  });
  await page.reload();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  await yaml.fill(definition("not-saved"));
  await expect(
    page
      .getByRole("alert")
      .filter({ hasText: "Browser storage is unavailable or full" }),
  ).toBeVisible();
  await expect(
    page.getByText("Draft could not be saved on this browser", { exact: true }),
  ).toBeVisible();
});
test("invite to successful agent workflow, administration, and responsive navigation", async ({
  page,
}) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(
    `/ui/accept-invite?token=${encodeURIComponent(process.env.UI_TEST_INVITE!)}`,
  );
  await expect(page.getByText("browser@example.com")).toBeVisible();
  await shot(page, "01-accept-invite");
  await page.getByLabel("Your name").fill("Browser Tester");
  await page
    .getByLabel("Password", { exact: true })
    .fill("browser-long-password");
  await page
    .getByRole("button", { name: "Accept invite", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Workflows", exact: true }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/ui\/workflows$/);
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Agents", exact: true })
    .click();
  await expect(page).toHaveURL(/\/ui\/agents$/);
  await page.goBack();
  await expect(
    page.getByRole("heading", { name: "Workflows", exact: true }),
  ).toBeVisible();
  await page.goForward();
  await expect(
    page.getByRole("heading", { name: "Agents", exact: true }),
  ).toBeVisible();
  await shot(page, "02-agents");
  await page.getByRole("button", { name: "New agent", exact: true }).click();
  await page.getByLabel("Agent name", { exact: true }).fill("browser-agent");
  await page
    .getByLabel("Provider account", { exact: true })
    .selectOption("browser-provider");
  await page.getByLabel("Model", { exact: true }).fill("fake-model");
  await page
    .getByLabel("Instructions", { exact: true })
    .fill("Give a concise answer to the user.");
  await page.getByRole("button", { name: "Save agent", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "browser-agent", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Use in workflow", exact: true })
    .click();
  // The agent arrives as the first step; its details are typed beside the canvas.
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("browser-workflow");
  // Edit a valid draft and publish immediately, before the field debounce fires.
  await expect(
    page.getByRole("button", { name: "Publish version", exact: true }),
  ).toBeEnabled();
  await page
    .getByLabel("Agent prompt", { exact: true })
    .fill("Summarize the test.");
  await page.getByLabel("Override model", { exact: true }).check();
  await page.getByLabel("Model", { exact: true }).fill("fake-model-override");
  await expect(
    page.getByLabel("Provider account", { exact: true }),
  ).toBeDisabled();
  const publication = page.waitForRequest(
    (request) =>
      request.method() === "POST" &&
      new URL(request.url()).pathname === "/be/v1/workflows",
  );
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  const publishedYAML = (await publication).postDataJSON().yaml;
  expect(publishedYAML).toContain("Summarize the test.");
  expect(publishedYAML).toContain("model: fake-model-override");
  expect(publishedYAML).not.toContain("connection:");
  const unchangedAgent = await (
    await page.request.get("/be/v1/agents/browser-agent")
  ).json();
  expect(unchangedAgent.model).toBe("fake-model");
  expect(unchangedAgent.connection).toBe("browser-provider");
  await expect(
    page.getByRole("heading", { name: "Run browser-workflow", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await expect(page.locator(".run-detail")).toContainText("short summary");
  await expect(page.locator(".timeline")).toContainText("run.succeeded");
  await shot(page, "03-run");
  await expect(
    page.getByRole("navigation").getByRole("button", { name: "Platform" }),
  ).toHaveCount(0);
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Vault", exact: true })
    .click();
  await page.getByRole("button", { name: "Add or replace secret" }).click();
  await page.getByLabel("Secret name", { exact: true }).fill("BROWSER_SECRET");
  await page
    .getByLabel("Value", { exact: true })
    .fill("browser-secret-write-only");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("BROWSER_SECRET", { exact: true })).toBeVisible();
  await expect(page.locator("body")).not.toContainText(
    "browser-secret-write-only",
  );
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Webhooks", exact: true })
    .click();
  await page.getByRole("button", { name: "New webhook", exact: true }).click();
  await page.getByLabel("Webhook name", { exact: true }).fill("browser-hook");
  await page
    .getByLabel("Workflow", { exact: true })
    .selectOption("browser-workflow");
  await page
    .getByRole("button", { name: "Create webhook", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Webhook credentials", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Dismiss", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "browser-hook", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Disable", exact: true }).click();
  await expect(page.locator(".status")).toHaveText("disabled");
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "API keys", exact: true })
    .click();
  await page.getByLabel("Name", { exact: true }).fill("browser-key");
  await page.getByRole("button", { name: "Create key", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "API key", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Dismiss", exact: true }).click();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Team", exact: true })
    .click();
  await expect(
    page.getByText("browser@example.com", { exact: true }),
  ).toBeVisible();
  await page.getByLabel("Email", { exact: true }).fill("teammate@example.com");
  await page
    .getByRole("button", { name: "Create invite", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Invite link", exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("Invite link", { exact: true })).toHaveValue(
    /\/ui\/accept-invite\?token=/,
  );
  await shot(page, "04-team-invite");
  await page.getByRole("button", { name: "Dismiss", exact: true }).click();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Usage & plans", exact: true })
    .click();
  await expect(
    page.getByText("Runs this month", { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: "/tmp/workflow-ui-desktop.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "Toggle navigation" }).click();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Workflows", exact: true }),
  ).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true);
  await page.screenshot({
    path: "/tmp/workflow-ui-mobile.png",
    fullPage: true,
  });
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Workflows", exact: true }),
  ).toBeVisible();
  expect(errors).toEqual([]);
});

test("typed agent inputs, provider test-before-save, live validation and version pinning", async ({
  page,
}) => {
  await page.goto("/ui/");
  await page.getByLabel("Email", { exact: true }).fill("browserco@example.com");
  await page
    .getByLabel("Password", { exact: true })
    .fill("owner-long-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "AI providers", exact: true })
    .click();
  // Providers have their own page, with every provider omp supports.
  await page.getByRole("button", { name: "Add provider", exact: true }).click();
  for (const name of ["Anthropic", "OpenAI", "Groq", "OpenRouter"])
    await expect(page.getByRole("radio", { name, exact: true })).toBeVisible();
  await page.getByLabel("Search providers", { exact: true }).fill("olla");
  await page.getByRole("radio", { name: "Ollama", exact: true }).click();
  // A local Ollama needs no key; it runs wherever its base URL points.
  await expect(page.getByLabel("Base URL", { exact: true })).toHaveValue(
    "http://127.0.0.1:11434/v1",
  );
  await page
    .getByLabel("Connection name", { exact: true })
    .fill("browser-second-provider");
  await page
    .getByLabel("Base URL", { exact: true })
    .fill(process.env.UI_TEST_PROVIDER_URL!);
  await shot(page, "07-provider-form");
  await page
    .getByRole("button", { name: "Test and save", exact: true })
    .click();
  await expect(page.locator(".notice")).toContainText("fake-model");
  const provider = page.locator("article").filter({
    has: page.getByRole("heading", {
      name: "browser-second-provider",
      exact: true,
    }),
  });
  await expect(provider).toContainText("Ollama");
  await shot(page, "06-providers");
  // Connectors hold apps and databases only.
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Connectors", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "browser-database" }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "browser-second-provider" }),
  ).toHaveCount(0);
  await shot(page, "06-connectors");
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Agents", exact: true })
    .click();
  await page.getByRole("button", { name: "New agent", exact: true }).click();
  await page.getByLabel("Agent name", { exact: true }).fill("typed-agent");
  await page
    .getByLabel("Provider account", { exact: true })
    .selectOption("browser-second-provider");
  await page.getByLabel("Model", { exact: true }).fill("fake-model");
  await page
    .getByLabel("Instructions", { exact: true })
    .fill("Summarize text as JSON with a summary field.");
  await page.getByText("Schemas and execution limits", { exact: true }).click();
  await page.getByLabel("Input schema (JSON)", { exact: true }).fill(
    JSON.stringify({
      type: "object",
      required: ["text"],
      properties: { text: { type: "string" } },
    }),
  );
  await page.getByLabel("Output schema (JSON)", { exact: true }).fill(
    JSON.stringify({
      type: "object",
      required: ["summary"],
      properties: { summary: { type: "string" } },
    }),
  );
  await page.getByRole("button", { name: "Save agent", exact: true }).click();
  const card = page.locator("article").filter({
    has: page.getByRole("heading", { name: "typed-agent", exact: true }),
  });
  await card.getByRole("button", { name: "Test agent", exact: true }).click();
  await page
    .getByLabel("Text", { exact: true })
    .fill("This is the test input.");
  await page
    .getByRole("button", { name: "Run agent test", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Agent completed", exact: true }),
  ).toBeVisible();
  await card
    .getByRole("button", { name: "Use in workflow", exact: true })
    .click();
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("typed-workflow");
  await page
    .getByLabel("Text", { exact: true })
    .fill("Run with schema-driven fields.");
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Run typed-workflow", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("editor-workflow");
  // A step with details missing is flagged and cannot be published.
  await addStep(page, "Transform");
  await expect(
    page.getByRole("status").filter({ hasText: "to fix" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Publish version", exact: true }),
  ).toBeDisabled();
  await page.getByLabel("Settings", { exact: true }).fill("value: first");
  await expect(
    page.getByRole("status").filter({ hasText: "Valid workflow" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(page.getByLabel("Version", { exact: true })).toHaveValue("1");
  // A second version adds a step; the first version stays pinned.
  await addStep(page, "No-op");
  await expect(
    page.getByRole("status").filter({ hasText: "Valid workflow" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(page.getByLabel("Version", { exact: true })).toHaveValue("2");
  await page.getByLabel("Version", { exact: true }).selectOption("1");
  await expect(page.getByLabel("Workflow diagram")).toContainText(
    "transform_1",
  );
  await expect(page.getByLabel("Workflow diagram")).not.toContainText("noop_2");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail")).toContainText("editor-workflow v1");
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
});

test("shared connectors, explicit agent permissions, and visual workflow builder", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto("/ui/");
  await page.getByLabel("Email", { exact: true }).fill("browserco@example.com");
  await page
    .getByLabel("Password", { exact: true })
    .fill("owner-long-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Connectors", exact: true })
    .click();
  await expect(page).toHaveURL(/\/ui\/connectors$/);
  for (const name of ["browser-database", "browser-slack", "browser-notion"])
    await expect(
      page.getByRole("heading", { name, exact: true }),
    ).toBeVisible();
  const database = page.locator("article").filter({
    has: page.getByRole("heading", { name: "browser-database", exact: true }),
  });
  await expect(database).toContainText("PostgreSQL database");
  await expect(database.getByRole("list", { name: "Actions" })).toContainText(
    "Query database (read only)Read only",
  );
  await page
    .getByRole("button", { name: "Add connector", exact: true })
    .click();
  await page
    .getByRole("radio", { name: "MongoDB database", exact: true })
    .click();
  await expect(
    page.getByLabel("Connection URI", { exact: true }),
  ).toHaveAttribute("type", "password");
  await expect(page.getByLabel("Encryption", { exact: true })).toHaveValue(
    "verify-full",
  );
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Agents", exact: true })
    .click();
  await page.getByRole("button", { name: "New agent", exact: true }).click();
  await page.getByLabel("Agent name", { exact: true }).fill("connector-reader");
  await page
    .getByLabel("Provider account", { exact: true })
    .selectOption("browser-provider");
  await page.getByLabel("Model", { exact: true }).fill("fake-model");
  await page
    .getByLabel("Instructions", { exact: true })
    .fill("Analyze the saved database using read actions.");
  // An agent searches whole connectors with glob, grep and read.
  await expect(page.getByText("glob, grep, read")).toBeVisible();
  await page.getByLabel(/^browser-database/).check();
  await expect(page.getByLabel(/^browser-slack/)).not.toBeChecked();
  await page.getByRole("button", { name: "Save agent", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "connector-reader", exact: true }),
  ).toBeVisible();
  const saved = await page.request.get("/be/v1/agents/connector-reader");
  expect((await saved.json()).tools).toEqual(["browser-database"]);
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  // Steps on the left, the diagram on the right.
  await expect(page.getByRole("region", { name: "Steps" })).toBeVisible();
  await expect(page.getByLabel("Workflow diagram")).toBeVisible();
  await page.getByLabel("Workflow name", { exact: true }).fill("visual-basics");
  await shot(page, "10-builder-empty");

  await addStep(page, "Transform");
  // An empty transform is not valid yet, and the message names the step.
  await expect(page.locator(".check-state")).toContainText("to fix");
  await page.getByRole("button", { name: /to fix/ }).click();
  await expect(
    page.getByRole("list", { name: "Problems" }).first(),
  ).toContainText("transform_1");
  await page.getByRole("button", { name: /to fix/ }).click();
  // Details are plain text, applied as you type; the canvas follows.
  await page
    .getByLabel("Settings", { exact: true })
    .fill("value:\n  greeting: hello");
  await addStep(page, "Sleep");
  await expect(page.getByText("After transform_1")).toBeVisible();
  await page.getByLabel("Settings", { exact: true }).fill("duration: 1s");
  await expect(page.getByLabel("Workflow diagram")).toContainText("1s");
  await expect(page.getByRole("region", { name: "Steps" })).toContainText("1s");
  await expect(
    page.getByRole("status").filter({ hasText: "Valid workflow" }),
  ).toBeVisible();
  await shot(page, "11-builder-two-steps");

  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Run visual-basics", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
    { timeout: 20000 },
  );
  await shot(page, "12-run-finished");

  // Reopen it: the published workflow loads into the same builder, and the
  // back link returns to the list without losing anything.
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await page.getByRole("button", { name: /visual-basics/ }).click();
  await expect(page.getByLabel("Workflow diagram")).toContainText(
    "transform_1",
  );
  await expect(page.getByLabel("Workflow diagram")).toContainText("sleep_2");
  // Choosing a step on the diagram opens its details in the list.
  await page.locator(".react-flow__node", { hasText: "transform_1" }).click();
  await expect(page.getByLabel("Settings", { exact: true })).toHaveValue(
    /greeting: hello/,
  );
  await shot(page, "13-builder-reopened");
  // On a phone the steps come first and the diagram shows only on request.
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByLabel("Workflow diagram")).toBeHidden();
  await page.getByRole("button", { name: "Show diagram" }).click();
  await expect(page.getByLabel("Workflow diagram")).toBeVisible();
  await expect
    .poll(() =>
      page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    )
    .toBe(true);
  await shot(page, "15-builder-phone");
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.getByRole("button", { name: "All workflows" }).click();
  await expect(
    page.getByRole("button", { name: /visual-basics/ }),
  ).toBeVisible();
  expect(errors).toEqual([]);
});
test("a secret group holds several keys and a workflow step reads one of them", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  const apiKey = "sk-group-api-key-123456";
  await signInOwner(page);
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Vault", exact: true })
    .click();
  await page.getByRole("button", { name: "Add or replace secret" }).click();
  await page.getByRole("button", { name: "Group of keys" }).click();
  await page
    .getByLabel("Group name", { exact: true })
    .fill("open_api_settings");
  const keys = [
    ["api_key", apiKey],
    ["effort", "high"],
    ["temperature", "0.3"],
  ];
  for (const [i, [key, value]] of keys.entries()) {
    if (i > 0) await page.getByRole("button", { name: "Add key" }).click();
    await page.locator('input[name="key"]').nth(i).fill(key);
    await page.locator('input[name="value"]').nth(i).fill(value);
  }
  await shot(page, "08-vault-group-form");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  // The group lists its keys with the reference to use; values are never shown.
  await expect(
    page.getByRole("heading", { name: "open_api_settings", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("${secrets.open_api_settings.api_key}"),
  ).toBeVisible();
  await expect(page.locator("body")).not.toContainText(apiKey);
  await shot(page, "09-vault-group");

  // A step picks a key from the group instead of anyone typing the reference.
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page.getByLabel("Workflow name", { exact: true }).fill("uses-group");
  await addStep(page, "HTTP request");
  const settings = page.getByLabel("Settings", { exact: true });
  await settings.fill(
    `method: POST\nurl: ${process.env.UI_TEST_PROVIDER_URL}/v1/chat\nbody:\n  effort: \${secrets.open_api_settings.effort}\nheaders:\n  Authorization: Bearer `,
  );
  await page.getByRole("button", { name: "Insert secret" }).click();
  await page.getByLabel("Search secrets").fill("api_key");
  await page.getByRole("listbox").getByRole("option").first().click();
  await expect(settings).toHaveValue(
    /Authorization: Bearer \$\{secrets\.open_api_settings\.api_key\}/,
  );
  await expect(
    page.getByRole("status").filter({ hasText: "Valid workflow" }),
  ).toBeVisible();
  await shot(page, "14-builder-secret");
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
    { timeout: 20000 },
  );
  // The stored input shows the credential masked and the short setting as is.
  await page.locator(".node-attempt summary").first().click();
  await expect(page.locator(".node-attempt").first()).toContainText(
    "Bearer ***",
  );
  await expect(page.locator(".node-attempt").first()).toContainText("high");
  await expect(page.locator(".run-detail")).not.toContainText(apiKey);
  expect(errors).toEqual([]);
});
test("workflow inputs become run fields and a person approves a task", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await signInOwner(page);
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page.getByLabel("Workflow name", { exact: true }).fill("approval-flow");
  // The Start row declares what each run is given.
  await openStart(page);
  await page.getByRole("button", { name: "Add variable", exact: true }).click();
  const amount = page.getByRole("group", { name: "Variable 1", exact: true });
  await amount.getByLabel("Name", { exact: true }).fill("amount");
  await amount.getByLabel("Type", { exact: true }).selectOption("number");
  await page.getByRole("button", { name: "Add variable", exact: true }).click();
  await page
    .getByRole("group", { name: "Variable 2", exact: true })
    .getByLabel("Name", { exact: true })
    .fill("reason");
  await expect(page.getByRole("button", { name: /^Start/ })).toContainText(
    "Inputs: amount, reason",
  );
  await addStep(page, "Human task");
  await page
    .getByLabel("Settings", { exact: true })
    .fill(
      "queue: finance\npayload:\n  amount: ${input.amount}\n  reason: ${input.reason}",
    );
  await addStep(page, "Transform");
  // An expression with ": " inside needs no YAML quoting.
  await page
    .getByLabel("Settings", { exact: true })
    .fill(
      "value:\n  decision: ${nodes.human_task_1.output.approved ? 'approved' : 'rejected'}",
    );
  await expect(
    page.getByRole("status").filter({ hasText: "Valid workflow" }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await page.getByLabel("Amount", { exact: true }).fill("120");
  await page.getByLabel("Reason", { exact: true }).fill("Team lunch");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "waiting",
  );
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Human tasks", exact: true })
    .click();
  const task = page.locator("section.task").filter({ hasText: "Team lunch" });
  await expect(task).toContainText("120");
  await task.getByLabel("Note", { exact: true }).fill("Within budget");
  await shot(page, "16-human-task");
  await task.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(task.locator(".status")).toHaveText("completed");
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Runs", exact: true })
    .click();
  await page
    .getByRole("button", { name: /approval-flow/ })
    .first()
    .click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await expect(page.locator(".run-result")).toContainText("approved");
  await shot(page, "17-run-result");
  expect(errors).toEqual([]);
});
test("a platform administrator invites a customer organization", async ({
  page,
}) => {
  await page.goto("/ui/");
  await shot(page, "00-sign-in");
  await page.getByLabel("Email", { exact: true }).fill("root@example.com");
  await page
    .getByLabel("Password", { exact: true })
    .fill(process.env.UI_TEST_ADMIN_PASSWORD!);
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Platform", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Platform", exact: true }),
  ).toBeVisible();
  await page
    .getByLabel("Owner email", { exact: true })
    .fill("customer@example.com");
  await page
    .getByLabel("Organization name", { exact: true })
    .fill("Customer Co");
  await page
    .getByRole("button", { name: "Create organization invite", exact: true })
    .click();
  await expect(page.getByLabel("Invite link", { exact: true })).toHaveValue(
    /\/ui\/accept-invite\?token=/,
  );
  await expect(
    page.getByRole("cell", { name: "Browser Co", exact: true }),
  ).toBeVisible();
  await shot(page, "05-platform");
});

test("set up two client accounts inside a fresh workflow and keep its draft", async ({
  page,
}) => {
  await page.goto("/ui/");
  await page
    .getByLabel("Email", { exact: true })
    .fill("freshsetup@example.com");
  await page
    .getByLabel("Password", { exact: true })
    .fill("owner-long-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("two-client-workflow");
  await addStep(page, "AI step");
  await page.getByLabel("Model", { exact: true }).fill("fake-model");
  await page
    .getByLabel("Agent prompt", { exact: true })
    .fill("Keep this prompt while connecting accounts.");
  await page
    .getByLabel("Instructions", { exact: true })
    .fill("Keep these instructions too.");
  for (const name of ["client-one", "client-two"]) {
    await page
      .getByRole("button", { name: "Add provider account", exact: true })
      .click();
    const dialog = page.getByRole("dialog", {
      name: "Add provider account",
      exact: true,
    });
    await dialog.getByLabel("Search providers").fill("compatible");
    await dialog
      .getByRole("radio", { name: "Custom (OpenAI-compatible)", exact: true })
      .click();
    await dialog.getByLabel("Connection name", { exact: true }).fill(name);
    await dialog
      .getByLabel("Base URL", { exact: true })
      .fill(process.env.UI_TEST_PROVIDER_URL!);
    await dialog
      .getByLabel("API key", { exact: true })
      .fill(`${name}-fake-key`);
    await dialog
      .getByRole("button", { name: "Test and save", exact: true })
      .click();
    await expect(dialog).toHaveCount(0);
    await expect(page).toHaveURL(/\/ui\/workflows$/);
    await expect(
      page.getByLabel("Provider account", { exact: true }),
    ).toHaveValue(name);
    await expect(page.getByLabel("Model", { exact: true })).toHaveValue(
      "fake-model",
    );
    await expect(page.getByLabel("Agent prompt", { exact: true })).toHaveValue(
      "Keep this prompt while connecting accounts.",
    );
  }
  await page
    .getByRole("button", { name: "Add provider account", exact: true })
    .click();
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Close setup" })
    .press("Escape");
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Add provider account", exact: true }),
  ).toBeFocused();
  // Closing a pending save must abort it rather than update this draft later.
  let releaseSave!: () => void;
  const heldSave = new Promise<void>((resolve) => {
    releaseSave = resolve;
  });
  await page.route("**/be/v1/providers", async (route) => {
    if (route.request().method() !== "POST") {
      await route.continue();
      return;
    }
    await heldSave;
    await route
      .fulfill({
        status: 201,
        json: {
          id: "late-account",
          name: "cancelled-account",
          provider: "openai_compatible",
        },
      })
      .catch(() => {});
  });
  await page
    .getByRole("button", { name: "Add provider account", exact: true })
    .click();
  const cancelled = page.getByRole("dialog", {
    name: "Add provider account",
    exact: true,
  });
  await cancelled
    .getByLabel("Connection name", { exact: true })
    .fill("cancelled-account");
  await cancelled
    .getByLabel("API key", { exact: true })
    .fill("fixture-only-key");
  const pendingSave = page.waitForRequest(
    (r) =>
      r.method() === "POST" && new URL(r.url()).pathname === "/be/v1/providers",
  );
  await cancelled
    .getByRole("button", { name: "Test and save", exact: true })
    .click();
  await pendingSave;
  const cancelledRequest = page.waitForEvent("requestfailed", {
    predicate: (r) =>
      r.method() === "POST" && new URL(r.url()).pathname === "/be/v1/providers",
  });
  await cancelled.getByRole("button", { name: "Close setup" }).click();
  await cancelledRequest;
  releaseSave();
  await page.unroute("**/be/v1/providers");
  await expect(
    page.getByLabel("Provider account", { exact: true }),
  ).toHaveValue("client-two");
  await expect(page.getByLabel("Agent prompt", { exact: true })).toHaveValue(
    "Keep this prompt while connecting accounts.",
  );
  await page.setViewportSize({ width: 390, height: 844 });
  await page
    .getByRole("button", { name: "Add provider account", exact: true })
    .click();
  const mobileDialog = page.getByRole("dialog", {
    name: "Add provider account",
    exact: true,
  });
  const bounds = await mobileDialog.boundingBox();
  expect(bounds!.x).toBeGreaterThanOrEqual(0);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(390);
  await shot(page, "07-mobile-provider-modal");
  await mobileDialog.getByRole("button", { name: "Close setup" }).click();
  await page.setViewportSize({ width: 1280, height: 720 });
  await shot(page, "07-inline-ai-accounts");
  const publication = page.waitForRequest(
    (r) =>
      r.method() === "POST" && new URL(r.url()).pathname === "/be/v1/workflows",
  );
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  const yaml = (await publication).postDataJSON().yaml;
  expect(yaml).toContain("connection: client-two");
  expect(yaml).toContain("model: fake-model");
  expect(yaml).not.toContain("fake-key");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("connector-draft");
  await page
    .getByRole("button", { name: "Add first step", exact: true })
    .first()
    .click();
  await page.getByLabel("Search steps and apps").fill("Send a Slack message");
  await page.getByRole("dialog").getByRole("option").first().click();
  const connector = page.getByRole("dialog", {
    name: "Connect Slack",
    exact: true,
  });
  await connector
    .getByLabel("Connection name", { exact: true })
    .fill("client-slack");
  await connector
    .getByLabel("Integration token", { exact: true })
    .fill("fake-slack-token");
  await shot(page, "08-inline-connector-modal");
  await connector
    .getByRole("button", { name: "Test and save", exact: true })
    .click();
  await expect(connector).toHaveCount(0);
  await expect(
    page.getByLabel("Connector account", { exact: true }),
  ).toHaveValue("client-slack");
  await expect(page.getByLabel("Workflow name", { exact: true })).toHaveValue(
    "connector-draft",
  );
  await page.getByRole("button", { name: "Add step", exact: true }).click();
  await page.getByLabel("Search steps and apps").fill("Gmail send");
  await page.getByRole("dialog").getByRole("option").first().click();
  const gmail = page.getByRole("dialog", {
    name: "Connect Gmail",
    exact: true,
  });
  await expect(gmail).toContainText("not enabled in this workspace yet");
  await expect(
    gmail.getByRole("button", { name: "Connect account", exact: true }),
  ).toBeDisabled();
  await gmail.getByRole("button", { name: "Close setup" }).click();
  await expect(page).toHaveURL(/\/ui\/workflows$/);
  await expect(page.getByLabel("Workflow name", { exact: true })).toHaveValue(
    "connector-draft",
  );
});

test("OAuth popup returns to the workflow without discarding its prompt", async ({
  page,
  context,
}) => {
  // Only external sign-in is simulated here; backend PKCE/state/token checks
  // have their own real HTTP integration coverage in TestOAuthConnectFlow.
  const connection = { name: "popup-gmail", kind: "gmail", config: {} };
  let connected = false;
  await context.route("**/be/v1/integrations", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    body.items.find(
      (k: { kind: string }) => k.kind === "gmail",
    ).oauth_configured = true;
    await route.fulfill({ response, json: body });
  });
  await context.route("**/be/v1/connections/oauth/start", async (route) => {
    connected = true;
    await route.fulfill({
      json: {
        authorize_url:
          process.env.UI_TEST_URL + "/ui/connectors?connected=popup-gmail",
      },
    });
  });
  await context.route("**/be/v1/connections/popup-gmail", async (route) => {
    await route.fulfill({ json: connection });
  });
  await context.route("**/be/v1/connections", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    if (connected) body.items.push(connection);
    await route.fulfill({ response, json: body });
  });
  await signInOwner(page);
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await page.getByLabel("Workflow name", { exact: true }).fill("oauth-draft");
  await addStep(page, "AI step");
  await page
    .getByLabel("Agent prompt", { exact: true })
    .fill("Keep my original task after OAuth.");
  await page.getByRole("button", { name: "Add step", exact: true }).click();
  await page.getByLabel("Search steps and apps").fill("Send Gmail");
  await page.getByRole("dialog").getByRole("option").first().click();
  const dialog = page.getByRole("dialog", {
    name: "Connect Gmail",
    exact: true,
  });
  await dialog
    .getByLabel("Connection name", { exact: true })
    .fill("popup-gmail");
  const popupPromise = page.waitForEvent("popup");
  await dialog
    .getByRole("button", { name: "Connect account", exact: true })
    .click();
  const popup = await popupPromise;
  await expect(dialog).toHaveCount(0);
  await expect.poll(() => popup.isClosed()).toBe(true);
  await expect(page).toHaveURL(/\/ui\/workflows$/);
  await expect(
    page.getByLabel("Connector account", { exact: true }),
  ).toHaveValue("popup-gmail");
  await expect(page.getByLabel("Workflow name", { exact: true })).toHaveValue(
    "oauth-draft",
  );
  await page.getByRole("button", { name: /^(?:1 )?agent_1(?: |$)/ }).click();
  await expect(page.getByLabel("Agent prompt", { exact: true })).toHaveValue(
    "Keep my original task after OAuth.",
  );
});

test("review creator steps before accepting and open only one detail at a time", async ({
  page,
}) => {
  await signInOwner(page);
  await page
    .getByRole("button", { name: "Create with AI", exact: true })
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("GLM 5.3 Flash · OpenRouter");
  await expect(dialog.getByRole("combobox")).toHaveCount(0);
  await dialog
    .getByLabel("Workflow context", { exact: true })
    .fill("Make a greeting and pass it to a second transform.");
  await dialog
    .getByRole("button", { name: "Propose first step", exact: true })
    .click();
  await expect(
    dialog.getByRole("heading", { name: "first · transform", exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("Workflow name", { exact: true })).toHaveCount(
    0,
  );
  await dialog
    .getByRole("button", { name: "Accept step", exact: true })
    .click();
  await expect(dialog.getByRole("status")).toHaveText("1 accepted step");
  await expect(
    dialog.getByRole("button", { name: "Accept step", exact: true }),
  ).toHaveCount(0);
  await dialog
    .getByRole("button", { name: "Propose next step", exact: true })
    .click();
  await expect(
    dialog.getByRole("heading", { name: "second · transform", exact: true }),
  ).toBeVisible();
  await expect(
    dialog.getByLabel("Workflow context", { exact: true }),
  ).toHaveValue("Make a greeting and pass it to a second transform.");
  await shot(page, "13-step-review");
  await dialog
    .getByRole("button", { name: "Accept step", exact: true })
    .click();
  await expect(dialog.getByRole("status")).toHaveText("2 accepted steps");
  await dialog
    .getByRole("button", { name: "Finish editing", exact: true })
    .click();
  const rows = page.locator(".step-row");
  const openRows = page.locator('.step-row[aria-expanded="true"]');
  await expect(openRows).toHaveCount(1);
  await page.getByRole("button", { name: /^1 first / }).click();
  await expect(openRows).toHaveCount(1);
  await expect(page.getByLabel("Settings", { exact: true })).toHaveValue(
    "value: hello",
  );
  await openStart(page);
  await expect(openRows).toHaveCount(1);
  await expect(page.getByLabel("Settings", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: /^2 second / }).click();
  await expect(openRows).toHaveCount(1);
  await expect(page.getByRole("button", { name: /^Start/ })).toHaveAttribute(
    "aria-expanded",
    "false",
  );
  await page.getByRole("button", { name: /^2 second / }).click();
  await expect(openRows).toHaveCount(0);
  await page.getByRole("button", { name: /^1 first / }).click();
  await expect(openRows).toHaveCount(1);
  await expect(rows).toHaveCount(3);
  await shot(page, "14-step-accordion");
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  const definition = parse(
    await page.getByLabel("Workflow YAML", { exact: true }).inputValue(),
  );
  expect(definition.nodes).toEqual([
    { id: "first", type: "transform", config: { value: "hello" } },
    {
      id: "second",
      type: "transform",
      needs: ["first"],
      config: { value: "${nodes.first.output}" },
    },
  ]);
  await page.reload();
  await page.getByRole("button", { name: "YAML", exact: true }).click();
  expect(
    parse(await page.getByLabel("Workflow YAML", { exact: true }).inputValue())
      .nodes,
  ).toEqual(definition.nodes);
});

test("all providers appear in agent and workflow account selection", async ({
  page,
}) => {
  await signInOwner(page, "freshsetup@example.com");
  await page.getByRole("button", { name: "Agents", exact: true }).click();
  await page.getByRole("button", { name: "New agent", exact: true }).click();
  const account = page.getByLabel("Provider account", { exact: true });
  await expect(account.locator("optgroup")).toHaveCount(22);
  await account.selectOption("add:openrouter");
  const dialog = page.getByRole("dialog");
  await expect(
    dialog.getByRole("radio", { name: "OpenRouter", exact: true }),
  ).toHaveAttribute("aria-checked", "true");
  await dialog
    .getByRole("button", { name: "Close setup", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Create an agent", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Workflows", exact: true }).click();
  await page.getByRole("button", { name: "New workflow", exact: true }).click();
  await addStep(page, "AI step");
  await expect(account.locator("optgroup")).toHaveCount(22);
  await account.selectOption("add:deepseek");
  await expect(
    dialog.getByRole("radio", { name: "DeepSeek", exact: true }),
  ).toHaveAttribute("aria-checked", "true");
  await dialog
    .getByRole("button", { name: "Close setup", exact: true })
    .click();
  await expect(page.getByLabel("Workflow name", { exact: true })).toBeVisible();
  await expect(account).toHaveValue("");
});

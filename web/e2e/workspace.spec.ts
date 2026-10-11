import { test, expect } from "@playwright/test";
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
  await page.getByRole("button", { name: "New agent", exact: true }).click();
  await page.getByLabel("Agent name", { exact: true }).fill("browser-agent");
  await page
    .getByLabel("Provider connection", { exact: true })
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
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("browser-workflow");
  await page
    .getByRole("button", { name: "Publish workflow", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Run browser-workflow", exact: true }),
  ).toBeVisible();
  await page.getByLabel("prompt", { exact: true }).fill("Summarize the test.");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await expect(page.locator(".run-detail")).toContainText("short summary");
  await expect(page.locator(".timeline")).toContainText("run.succeeded");
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

test("typed agent inputs, provider test-before-save, live YAML validation and version pinning", async ({
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
    .getByRole("button", { name: "Connectors", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Add connector", exact: true })
    .click();
  await page.getByLabel("Integration", { exact: true }).selectOption("llm");
  await page
    .getByLabel("Connection name", { exact: true })
    .fill("browser-second-provider");
  await page
    .getByLabel("Provider", { exact: true })
    .selectOption("openai_compatible");
  await page
    .getByLabel("API key", { exact: true })
    .fill("browser-only-test-key");
  await page
    .getByLabel("Base URL", { exact: true })
    .fill(process.env.UI_TEST_PROVIDER_URL!);
  await page
    .getByRole("button", { name: "Test and save", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "browser-second-provider", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Agents", exact: true })
    .click();
  await page.getByRole("button", { name: "New agent", exact: true }).click();
  await page.getByLabel("Agent name", { exact: true }).fill("typed-agent");
  await page
    .getByLabel("Provider connection", { exact: true })
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
    .getByLabel("text", { exact: true })
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
    .getByRole("button", { name: "Publish workflow", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Run typed-workflow", exact: true }),
  ).toBeVisible();
  await page
    .getByLabel("text", { exact: true })
    .fill("Run with schema-driven fields.");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await page
    .getByRole("button", { name: "New YAML workflow", exact: true })
    .click();
  const editor = page.locator(".cm-content");
  await editor.fill(
    "apiVersion: workflow/v1\nname: editor-workflow\nnodes:\n - id: broken\n   type: nonexistent\n   config: {}\n",
  );
  await expect(
    page.getByText("Workflow needs attention", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Publish version", exact: true }),
  ).toBeDisabled();
  const v1 =
    "apiVersion: workflow/v1\nname: editor-workflow\nnodes:\n - id: first\n   type: noop\n   config: {}\n";
  await editor.fill(v1);
  await expect(
    page.getByText("✓ Valid workflow", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(page.getByLabel("Version", { exact: true })).toHaveValue("1");
  await editor.fill(v1.replace("id: first", "id: second"));
  await expect(
    page.getByText("✓ Valid workflow", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(page.getByLabel("Version", { exact: true })).toHaveValue("2");
  await page.getByLabel("Version", { exact: true }).selectOption("1");
  await expect(editor).toContainText("id: first");
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
  await expect(
    page.getByText("Query database (read only) · Read only", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Add connector", exact: true })
    .click();
  await page.getByLabel("Integration", { exact: true }).selectOption("mongo");
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
    .getByLabel("Provider connection", { exact: true })
    .selectOption("browser-provider");
  await page.getByLabel("Model", { exact: true }).fill("fake-model");
  await page
    .getByLabel("Instructions", { exact: true })
    .fill("Analyze the saved database using read actions.");
  await page
    .getByLabel("Query database (read only)Read only", { exact: true })
    .check();
  await expect(
    page.getByLabel("Insert database rowsWrites to this service", {
      exact: true,
    }),
  ).not.toBeChecked();
  await page.getByRole("button", { name: "Save agent", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "connector-reader", exact: true }),
  ).toBeVisible();
  const saved = await page.request.get("/be/v1/agents/connector-reader");
  expect((await saved.json()).tools).toEqual([
    "connector:browser-database:db.query",
  ]);
  await page
    .getByRole("navigation")
    .getByRole("button", { name: "Workflows", exact: true })
    .click();
  await page
    .getByRole("button", { name: "New visual workflow", exact: true })
    .click();
  await page
    .getByLabel("Workflow name", { exact: true })
    .fill("visual-connectors");
  await page
    .getByLabel("Saved connector", { exact: true })
    .selectOption("browser-database");
  await page
    .getByLabel("Connector action", { exact: true })
    .selectOption("db.query");
  await page.getByRole("button", { name: "Add step", exact: true }).click();
  await page.getByLabel("sql", { exact: true }).fill("select 7 as n");
  await page
    .getByRole("button", { name: "Apply step settings", exact: true })
    .click();
  await page.getByLabel("Step type", { exact: true }).selectOption("agent");
  await page
    .getByLabel("Saved agent", { exact: true })
    .selectOption("connector-reader");
  await page.getByRole("button", { name: "Add step", exact: true }).click();
  await page
    .getByLabel("After step", { exact: true })
    .selectOption("connector_call_2");
  await page
    .getByLabel("Agent prompt", { exact: true })
    .fill("Summarize this query result: ${nodes.connector_call_2.output}");
  await page
    .getByRole("button", { name: "Apply step settings", exact: true })
    .click();
  await expect(
    page.getByRole("status").filter({ hasText: "Valid workflow" }),
  ).toBeVisible();
  await page.screenshot({
    path: "/tmp/workflow-connector-builder.png",
    fullPage: true,
  });
  await page
    .getByRole("button", { name: "Publish version", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Run visual-connectors", exact: true }),
  ).toBeVisible();
  await page.getByRole("tab", { name: "YAML", exact: true }).click();
  await expect(page.locator(".cm-content")).toContainText(
    "connection: browser-database",
  );
  await expect(page.locator(".cm-content")).toContainText("connector_call_2");
  await page.getByRole("button", { name: "Start run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  await expect(page.locator(".run-detail")).toContainText("connector_call_2");
  expect(errors).toEqual([]);
});

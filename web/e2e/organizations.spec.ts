import { test, expect } from "@playwright/test";

test("create isolated organizations and accept invitations for new and signed-in users", async ({
  page,
  browser,
}) => {
  const suffix = Date.now().toString();
  const name = `Client ${suffix}`;
  const email = `teammate-${suffix}@example.com`;
  const password = "invite-password-123";
  await page.goto("/ui/");
  await page.getByLabel("Email", { exact: true }).fill("browserco@example.com");
  await page
    .getByLabel("Password", { exact: true })
    .fill("owner-long-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page
    .getByRole("button", { name: "Organizations", exact: true })
    .click();
  await page.getByLabel("Organization name", { exact: true }).fill(name);
  await page
    .getByRole("button", { name: "Create organization", exact: true })
    .click();
  await expect(page.getByRole("status")).toContainText("Organization created");
  await page
    .getByRole("button", { name: `Switch to ${name}`, exact: true })
    .click();
  await expect(
    page.getByLabel("Organization", { exact: true }).locator("option:checked"),
  ).toHaveText(name);
  await page.getByRole("button", { name: "Connectors", exact: true }).click();
  await expect(page.getByText("browser-slack", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Team", exact: true }).click();
  await page.getByLabel("Email", { exact: true }).fill(email);
  await page
    .getByRole("button", { name: "Create invite", exact: true })
    .click();
  const firstLink = await page
    .getByLabel("Invite link", { exact: true })
    .inputValue();
  // PUBLIC_URL can differ in tests; follow the link's path on the test server.
  const invitePath = (link: string) =>
    new URL(link).pathname + new URL(link).search;
  const context = await browser.newContext({
    baseURL: process.env.UI_TEST_URL,
  });
  try {
    const teammate = await context.newPage();
    await teammate.goto(invitePath(firstLink));
    await teammate.getByLabel("Your name").fill("Teammate");
    await teammate.getByLabel("Password", { exact: true }).fill(password);
    await teammate
      .getByRole("button", { name: "Accept invite", exact: true })
      .click();
    await expect(teammate).toHaveURL(/\/ui\/workflows$/);
    await teammate
      .getByRole("button", { name: "Sign out", exact: true })
      .click();
    await teammate.getByLabel("Email", { exact: true }).fill(email);
    await teammate.getByLabel("Password", { exact: true }).fill(password);
    await teammate
      .getByRole("button", { name: "Sign in", exact: true })
      .click();
    await expect(
      teammate
        .getByLabel("Organization", { exact: true })
        .locator("option:checked"),
    ).toHaveText(name);
    await page
      .getByLabel("Organization", { exact: true })
      .selectOption({ label: "Browser Co" });
    await page.getByRole("button", { name: "Team", exact: true }).click();
    await page.getByLabel("Email", { exact: true }).fill(email);
    await page
      .getByRole("button", { name: "Create invite", exact: true })
      .click();
    const secondLink = await page
      .getByLabel("Invite link", { exact: true })
      .inputValue();
    await teammate.goto(invitePath(secondLink));
    await expect(
      teammate.getByRole("heading", { name: "Join your team" }),
    ).toBeVisible();
    await expect(
      teammate.getByText("Enter your existing account password."),
    ).toBeVisible();
    await expect(teammate.getByLabel("Your name")).toHaveCount(0);
    await teammate.getByLabel("Password", { exact: true }).fill(password);
    await teammate
      .getByRole("button", { name: "Accept invite", exact: true })
      .click();
    await expect(
      teammate
        .getByLabel("Organization", { exact: true })
        .locator("option:checked"),
    ).toHaveText("Browser Co");
    await expect(
      teammate.getByLabel("Organization", { exact: true }).locator("option"),
    ).toHaveCount(2);
    await teammate.goto(invitePath(secondLink));
    await expect(teammate.getByRole("alert")).toContainText(
      "invalid, expired or already used",
    );
    await expect(
      teammate.getByRole("button", { name: "Accept invite", exact: true }),
    ).toBeDisabled();
  } finally {
    await context.close();
  }
});

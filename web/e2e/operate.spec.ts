import { test, expect } from "@playwright/test";

test("create and test a webhook, deliver failure and recovery, and fire a real scheduled slot", async ({
  page,
}) => {
  test.setTimeout(100_000);
  await page.goto("/ui/");
  await page.getByLabel("Email", { exact: true }).fill("browserco@example.com");
  await page
    .getByLabel("Password", { exact: true })
    .fill("owner-long-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await page.getByRole("button", { name: "Alerts", exact: true }).click();
  await page
    .getByRole("button", { name: "New alert channel", exact: true })
    .click();
  await page
    .getByLabel("Channel name", { exact: true })
    .fill("browser-webhook");
  await page.getByLabel("Send to", { exact: true }).selectOption("webhook");
  await page
    .getByLabel("Endpoint URL", { exact: true })
    .fill(process.env.UI_TEST_ALERT_URL!);
  await page.getByLabel("browser-operate", { exact: true }).check();
  await page
    .getByRole("button", { name: "Create channel", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Webhook signing secret", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Dismiss", exact: true }).click();
  await page.getByRole("button", { name: "Send test", exact: true }).click();
  await expect(page.getByRole("status")).toContainText(
    "Test alert sent to browser-webhook.",
  );

  const start = async (url: string) => {
    const response = await page.request.post("/be/v1/runs", {
      data: { workflow: "browser-operate", input: { url } },
    });
    expect(response.status()).toBe(201);
    const run = await response.json();
    await expect
      .poll(
        async () =>
          (await (await page.request.get(`/be/v1/runs/${run.id}`)).json())
            .status,
      )
      .toMatch(/^(failed|succeeded)$/);
    return run.id;
  };
  await start("http://127.0.0.1:1/");
  const failed = page
    .locator(".delivery-log li")
    .filter({ hasText: "A workflow starts failing" });
  await expect
    .poll(async () => {
      await page.getByRole("button", { name: "Refresh", exact: true }).click();
      return failed.textContent();
    })
    .toContain("sent");
  const recoveredID = await start(process.env.UI_TEST_OPERATE_URL!);
  const recovered = page
    .locator(".delivery-log li")
    .filter({ hasText: "A failing workflow recovers" });
  await expect
    .poll(async () => {
      await page.getByRole("button", { name: "Refresh", exact: true }).click();
      return recovered.textContent();
    })
    .toContain("sent");
  await recovered
    .getByRole("button", { name: "View run", exact: true })
    .click();
  await expect(page).toHaveURL(new RegExp(`/ui/runs/${recoveredID}$`));
  await page.reload();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
  // An alert link opened while signed out returns to that run after login.
  await page.context().clearCookies();
  await page.reload();
  await page.getByLabel("Email", { exact: true }).fill("browserco@example.com");
  await page
    .getByLabel("Password", { exact: true })
    .fill("owner-long-password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`/ui/runs/${recoveredID}$`));
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );

  await page.getByRole("button", { name: "Schedules", exact: true }).click();
  await page.getByRole("button", { name: "New schedule", exact: true }).click();
  await page
    .getByLabel("Schedule name", { exact: true })
    .fill("browser-minute");
  await page
    .getByLabel("Workflow", { exact: true })
    .selectOption("browser-operate");
  await page.getByLabel("Cron expression", { exact: true }).fill("* * * * *");
  await page.getByLabel("Timezone", { exact: true }).selectOption("UTC");
  await page
    .getByLabel("URL", { exact: true })
    .fill(process.env.UI_TEST_OPERATE_URL!);
  await expect(page.locator(".next-runs li")).toHaveCount(5);
  await page
    .getByRole("button", { name: "Create schedule", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "browser-minute", exact: true }),
  ).toBeVisible();
  // Observe wall-clock cron dispatch by the real worker; no SQL clock override.
  await expect
    .poll(
      async () => {
        const response = await page.request.get("/be/v1/schedules");
        return (await response.json()).items.find(
          (s: { name: string }) => s.name === "browser-minute",
        ).last_outcome;
      },
      { timeout: 75_000, intervals: [500, 1000] },
    )
    .toBe("started");
  await page.reload();
  const card = page
    .locator(".schedule-card")
    .filter({ hasText: "browser-minute" });
  await expect(card).toContainText("Started a run");
  await card.getByRole("button", { name: "Turn off", exact: true }).click();
  await expect(
    card.getByRole("button", { name: "Turn on", exact: true }),
  ).toBeVisible();
  await card.getByRole("button", { name: "View run", exact: true }).click();
  await expect(page.locator(".run-detail .status").first()).toHaveText(
    "succeeded",
  );
});

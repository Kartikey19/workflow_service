import { defineConfig } from "@playwright/test";
export default defineConfig({
  testDir: "./e2e",
  workers: 1,
  timeout: 45000,
  use: {
    baseURL: process.env.UI_TEST_URL,
    browserName: "chromium",
    screenshot: "only-on-failure",
    trace: "off",
  },
  reporter: "line",
});

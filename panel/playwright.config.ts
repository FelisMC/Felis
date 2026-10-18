import { defineConfig, devices } from "@playwright/test";

// Mock state is shared, so serialize tests and reset it before each case.
// PW_BASE_URL points it at an already running mock server.
const port = 5298;
const external = process.env.PW_BASE_URL;

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: external ?? `http://localhost:${port}`,
    channel: "chrome",
    locale: "en-US",
    trace: "retain-on-failure",
  },
  projects: [
    { name: "desktop", testMatch: /(smoke|a11y)\.spec\.ts/, use: { ...devices["Desktop Chrome"], channel: "chrome" } },
    {
      name: "mobile",
      testMatch: /mobile\.spec\.ts/,
      use: { channel: "chrome", viewport: { width: 375, height: 812 }, hasTouch: true, isMobile: true },
    },
  ],
  webServer: external
    ? undefined
    : {
        command: `node node_modules/vite/bin/vite.js --mode mock --port ${port} --strictPort`,
        url: `http://localhost:${port}/`,
        reuseExistingServer: !process.env.CI,
        timeout: 60_000,
      },
});

// End-to-end tests for play.ryancaudill.dev. They run the real play server
// (TestFixtureServer in web/cmd/play) against the synthetic world, with one
// port per server state. See play.spec.js.
import { defineConfig, devices } from "@playwright/test";

export const PORT = Number(process.env.PLAY_E2E_PORT || 19150);

export default defineConfig({
  testDir: ".",
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: {
    baseURL: `http://127.0.0.1:${PORT}`,
    // Fixed locale and zone, so dates and numbers read the same everywhere.
    locale: "en-US",
    timezoneId: "UTC",
    // No entrance animations or count-ups, so screenshots are stable.
    reducedMotion: "reduce",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: {
    command: "go test ./cmd/play -run '^TestFixtureServer$' -count=1 -timeout 0 -v",
    cwd: "../../../web",
    env: { PLAY_FIXTURE_ADDR: `127.0.0.1:${PORT}` },
    // The last state's port, which opens with the rest.
    url: `http://127.0.0.1:${PORT + 3}/`,
    timeout: 180_000,
    reuseExistingServer: !process.env.CI,
    stdout: "pipe",
  },
});

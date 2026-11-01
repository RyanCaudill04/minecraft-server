// End-to-end tests for play.ryancaudill.dev against the synthetic world in
// web/internal/world/worldtest. Every player and stat there is made up.
import { mkdirSync } from "node:fs";
import { expect, test } from "@playwright/test";
import { PORT } from "./playwright.config.js";

// The fixture serves each server state on its own port.
const STATES = ["active", "sleeping", "starting", "offline"];
const urlFor = (state) => `http://127.0.0.1:${PORT + STATES.indexOf(state)}/`;

// A fixed "now" a few hours after the synthetic world's last save
// (2026-10-27 02:46 UTC), so "ago" text is stable.
const NOW = new Date("2026-10-27T12:00:00Z");

// Stands in for mc-heads.net, so runs are offline and deterministic.
const HEAD = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><rect width="8" height="8" fill="#6a8f3a"/><rect x="2" y="3" width="1" height="1" fill="#fff"/><rect x="5" y="3" width="1" height="1" fill="#fff"/></svg>`;

test.beforeEach(async ({ context, page }) => {
  await context.route(
    (url) => url.hostname !== "127.0.0.1",
    (route) =>
      new URL(route.request().url()).hostname === "mc-heads.net"
        ? route.fulfill({ contentType: "image/svg+xml", body: HEAD })
        : route.abort(),
  );
  await page.clock.setFixedTime(NOW);
});

// Opens the page in a state and waits for the API data to render.
async function open(page, state = "active") {
  await page.goto(urlFor(state));
  await expect(page.locator("#board li")).toHaveCount(3);
}

test.describe("server states", () => {
  const cases = [
    { state: "active", title: "Online", message: "The server is up. Jump in!", sub: "Alice" },
    { state: "sleeping", title: "Sleeping", message: /^Asleep since 2:46\sAM\.$/, sub: "Last played 15h ago by Bob" },
    { state: "starting", title: "Starting up", message: "Almost ready. Try joining in a moment.", sub: "Last played 15h ago by Bob" },
    { state: "offline", title: "Offline", message: "The server is down right now. Check back later.", sub: "Last played 15h ago by Bob" },
  ];
  for (const c of cases) {
    test(c.state, async ({ page }) => {
      await open(page, c.state);
      await expect(page.locator("#status-card")).toHaveAttribute("data-state", c.state);
      await expect(page.locator(".pill")).toHaveAttribute("data-state", c.state);
      await expect(page.locator("#pill-text")).toHaveText(c.title);
      await expect(page.locator("#state-title")).toHaveText(c.title);
      await expect(page.locator("#state-message")).toHaveText(c.message);
      await expect(page.locator("#online-sub")).toHaveText(c.sub);
    });
  }

  test("active shows who's online, minus hidden players", async ({ page }) => {
    await open(page, "active");
    await expect(page.locator("#online")).toHaveText("2");
    await expect(page.locator("#max")).toHaveText("/ 20");
    // The anonymous sample entry gets no avatar.
    await expect(page.locator("#online-avatars img")).toHaveCount(1);
    // Online players sort first in the grid.
    const first = page.locator("#player-grid .player-card").first();
    await expect(first.locator(".name")).toHaveText("Alice");
    await expect(first.locator(".online-badge")).toBeVisible();
  });

  test("an unreachable status API", async ({ page }) => {
    await page.route("**/api/status", (route) => route.fulfill({ status: 502, body: "" }));
    await open(page, "active");
    await expect(page.locator("#status-card")).toHaveAttribute("data-state", "unknown");
    await expect(page.locator("#state-title")).toHaveText("Can't reach status");
  });
});

test("world facts and the in-game clock", async ({ page }) => {
  await open(page);
  // Day 315, tick 5453.
  await expect(page.locator("#clock")).toHaveText("11:27 AM");
  await expect(page.locator("#clock-sub")).toHaveText("Day 315 · Noon · Rain");
  const fact = (label) => page.locator(".fact").filter({ has: page.getByText(label, { exact: true }) }).locator(".value");
  await expect(fact("Difficulty")).toHaveText("Easy");
  await expect(fact("Spawn")).toHaveText("192, 76, -192");
  await expect(fact("Version")).toHaveText("26.2");
  await expect(fact("Last saved")).toHaveText("9h ago");
  await expect(fact("Players")).toHaveText("3");
});

test("leaderboard tabs", async ({ page }) => {
  await open(page);
  const tabs = page.locator("#board-tabs [role=tab]");
  await expect(tabs).toHaveText(["Playtime", "Mob kills", "Blocks mined", "Distance", "Advancements", "Fish caught", "Deaths"]);
  await expect(tabs.first()).toHaveAttribute("aria-selected", "true");

  const rows = page.locator("#board li");
  await expect(rows.locator(".name")).toHaveText(["Alice", "Bob", "33333333"]);
  await expect(rows.first().locator(".score")).toHaveText("1h 0m");

  await page.getByRole("tab", { name: "Mob kills" }).click();
  await expect(page.getByRole("tab", { name: "Mob kills" })).toHaveAttribute("aria-selected", "true");
  await expect(tabs.first()).toHaveAttribute("aria-selected", "false");
  await expect(rows.locator(".score")).toHaveText(["40", "5", "0"]);

  await page.getByRole("tab", { name: "Distance" }).click();
  await expect(rows.first().locator(".score")).toHaveText("750 m");
});

test.describe("player sheet", () => {
  async function openAlice(page) {
    await open(page, "active");
    const card = page.locator("#player-grid .player-card").filter({ hasText: "Alice" });
    await card.click();
    const sheet = page.getByRole("dialog");
    await expect(sheet).toBeVisible();
    await expect(page.locator("#backdrop")).toBeVisible();
    return { card, sheet };
  }

  test("shows the player's stats", async ({ page }) => {
    const { sheet } = await openAlice(page);
    await expect(sheet.locator("#sheet-name")).toHaveText("Alice");
    await expect(sheet.locator("#sheet-seen")).toHaveText("Online now");
    const stat = (label) => sheet.locator(".sheet-stats > div").filter({ has: page.getByText(label, { exact: true }) }).locator("dd");
    await expect(stat("Playtime")).toHaveText("1h 0m#1 on the server");
    await expect(stat("Jumps")).toHaveText("300#1 on the server");
    // Most deaths isn't an honour.
    await expect(stat("Deaths")).toHaveText("2");
    await expect(page.locator("#sheet-close")).toBeFocused();
  });

  test("Esc closes it and returns focus", async ({ page }) => {
    const { card, sheet } = await openAlice(page);
    await page.keyboard.press("Escape");
    await expect(sheet).toBeHidden();
    await expect(page.locator("#backdrop")).toBeHidden();
    await expect(card).toBeFocused();
  });

  test("the close button closes it", async ({ page }) => {
    const { card, sheet } = await openAlice(page);
    await page.locator("#sheet-close").click();
    await expect(sheet).toBeHidden();
    await expect(card).toBeFocused();
  });

  test("clicking the backdrop closes it", async ({ page }) => {
    const { sheet } = await openAlice(page);
    await page.locator("#backdrop").click({ position: { x: 10, y: 10 } });
    await expect(sheet).toBeHidden();
  });

  test("leaderboard rows open it too", async ({ page }) => {
    await open(page, "sleeping");
    await page.locator("#board li").filter({ hasText: "Bob" }).click();
    await expect(page.locator("#sheet-name")).toHaveText("Bob");
    await expect(page.locator("#sheet-seen")).toHaveText("Last seen 15h ago");
  });
});

test("scroll spy follows the sections", async ({ page }) => {
  await open(page);
  const link = (hash) => page.locator(`.nav a[href="#${hash}"]`);
  await expect(link("overview")).toHaveClass(/active/);
  await link("leaderboards").click();
  await expect(link("leaderboards")).toHaveClass(/active/);
  await expect(link("overview")).not.toHaveClass(/active/);
  await page.evaluate(() => window.scrollTo(0, 0));
  await expect(link("overview")).toHaveClass(/active/);
});

test("copy button copies the address", async ({ context, page }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await open(page);
  await page.locator("#copy").click();
  await expect(page.locator("#copy")).toHaveText("Copied!");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe("mc.ryancaudill.dev");
});

// Previews for pull requests: CI uploads tests/play/e2e/screenshots.
test.describe("screenshots", () => {
  const sizes = { phone: { width: 390, height: 844 }, desktop: { width: 1280, height: 800 } };
  mkdirSync("screenshots", { recursive: true });

  for (const [size, viewport] of Object.entries(sizes)) {
    for (const scheme of ["light", "dark"]) {
      test(`${size} ${scheme}`, async ({ page }) => {
        await page.setViewportSize(viewport);
        await page.emulateMedia({ colorScheme: scheme });
        for (const state of STATES) {
          await open(page, state);
          // Wait for the stand-in heads, so they're in the shot.
          await page.waitForLoadState("networkidle");
          await page.screenshot({ path: `screenshots/${size}-${scheme}-${state}.png`, fullPage: true });
        }
        // Nothing may stick out sideways on a phone.
        const overflow = await page.evaluate(() => document.documentElement.scrollWidth - innerWidth);
        expect(overflow, "horizontal overflow in px").toBeLessThanOrEqual(0);

        await page.locator("#player-grid .player-card").first().click();
        await expect(page.getByRole("dialog")).toBeVisible();
        await page.waitForFunction(() => document.getElementById("sheet-head").naturalWidth > 0);
        await page.screenshot({ path: `screenshots/${size}-${scheme}-sheet.png` });
      });
    }
  }
});

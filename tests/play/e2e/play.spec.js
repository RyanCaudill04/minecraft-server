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
    { state: "active", title: "Online", message: "The server is up. Jump in!", sub: "Alice and 1 other" },
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

test("activity feed, newest first", async ({ page }) => {
  await open(page);
  const items = page.locator("#feed .feed-item");
  await expect(items).toHaveCount(9);
  await expect(items.locator(".feed-line")).toHaveText([
    "Alice earned Sky's the Limit",
    "Bob earned Sky's the Limit",
    "Bob earned The End?",
    "Alice earned The End?",
    "Bob earned Diamonds!",
    "Bob earned We Need to Go Deeper",
    "Bob joined for the first time",
    "Alice earned We Need to Go Deeper",
    "Alice joined for the first time",
  ]);
  const newest = items.first().locator("time");
  await expect(newest).toHaveText("7d ago · Oct 20, 2026, 12:00 PM");
  await expect(newest).toHaveAttribute("datetime", "2026-10-20T12:00:00Z");
  await expect(page.locator("#feed-empty")).toBeHidden();
});

test("leaderboard picker", async ({ page }) => {
  await open(page);
  const tabs = page.locator("#board-tabs button");
  await expect(tabs).toHaveText(["Playtime", "Mob kills", "Blocks mined", "Distance", "Advancements", "Fish caught", "Deaths"]);
  await expect(tabs.first()).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("#board")).toHaveAttribute("aria-label", "Playtime leaderboard");

  const rows = page.locator("#board li");
  await expect(rows.locator(".name")).toHaveText(["Alice", "Bob", "33333333"]);
  await expect(rows.first().locator(".score")).toHaveText("1h 0m");

  const mobKills = page.locator("#board-tabs").getByRole("button", { name: "Mob kills" });
  await mobKills.click();
  await expect(mobKills).toHaveAttribute("aria-pressed", "true");
  await expect(tabs.first()).toHaveAttribute("aria-pressed", "false");
  await expect(rows.locator(".score")).toHaveText(["40", "5", "0"]);

  await page.locator("#board-tabs").getByRole("button", { name: "Distance" }).click();
  await expect(rows.first().locator(".score")).toHaveText("750 m");
});

test.describe("player sheet", () => {
  async function openAlice(page) {
    await open(page, "active");
    const card = page.locator("#player-grid .player-card").filter({ hasText: "Alice" });
    await card.click();
    const sheet = page.getByRole("dialog");
    await expect(sheet).toBeVisible();
    expect(await sheet.evaluate((d) => d.matches(":modal"))).toBe(true);
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

  test("shows the player's skin and server firsts", async ({ page }) => {
    const { sheet } = await openAlice(page);
    await expect(sheet.locator("#sheet-skin")).toHaveAttribute("src", /^https:\/\/mc-heads\.net\/body\//);
    const badges = sheet.locator("#sheet-badges .badge");
    await expect(badges.locator(".badge-label")).toHaveText(["First into the Nether", "First into the End", "First wings"]);
    await expect(badges.locator(".badge-date")).toHaveText(["Oct 2, 2026", "Oct 8, 2026", "Oct 20, 2026"]);
  });

  test("hides server firsts for a player without any", async ({ page }) => {
    await open(page, "active");
    await page.locator("#player-grid .player-card").filter({ hasText: "33333333" }).click();
    await expect(page.locator("#sheet-name")).toHaveText("33333333");
    await expect(page.locator("#sheet-firsts")).toBeHidden();
  });

  test("Esc closes it and returns focus", async ({ page }) => {
    const { card, sheet } = await openAlice(page);
    await page.keyboard.press("Escape");
    await expect(sheet).toBeHidden();
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
    // The page behind is inert, so click the backdrop by position.
    await page.mouse.click(5, 5);
    await expect(sheet).toBeHidden();
  });

  test("leaderboard rows open it too", async ({ page }) => {
    await open(page, "sleeping");
    await page.locator("#board li").filter({ hasText: "Bob" }).click();
    await expect(page.locator("#sheet-name")).toHaveText("Bob");
    await expect(page.locator("#sheet-seen")).toHaveText("Last seen 15h ago");
  });

  test("leaderboard rows open from the keyboard and get focus back", async ({ page }) => {
    await open(page, "sleeping");
    const row = page.locator("#board li button").filter({ hasText: "Bob" });
    await row.focus();
    await page.keyboard.press("Enter");
    await expect(page.locator("#sheet-name")).toHaveText("Bob");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog")).toBeHidden();
    await expect(row).toBeFocused();
  });

  test("a click inside it keeps it open", async ({ page }) => {
    const { sheet } = await openAlice(page);
    await page.locator("#sheet-name").click();
    await expect(sheet).toBeVisible();
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

test("the Map group opens to its worlds and follows the scroll", async ({ page }) => {
  await open(page);
  const toggle = page.locator("#map-toggle");
  const nether = page.locator('.nav a[href="#map-nether"]');
  const group = page.locator("#map-nav");
  // A closed group is collapsed and inert, so its links can't be focused.
  await expect(toggle).toHaveAttribute("aria-expanded", "false");
  await expect(group).toHaveAttribute("inert", "");
  expect((await group.boundingBox()).height).toBe(0);

  await toggle.click();
  await expect(toggle).toHaveAttribute("aria-expanded", "true");
  await expect(group).not.toHaveAttribute("inert");
  await nether.click();
  await expect(nether).toHaveClass(/active/);
  await expect(toggle).toHaveClass(/active/);
  await expect(page.locator('.nav a[href="#map-overworld"]')).not.toHaveClass(/active/);
  // The End is the last section, so the scroll spy picks it at the bottom.
  const end = page.locator('.nav a[href="#map-end"]');
  await end.click();
  await expect(end).toHaveClass(/active/);
  await expect(toggle).toHaveClass(/active/);

  await toggle.click();
  await expect(group).toHaveAttribute("inert", "");
  await page.evaluate(() => window.scrollTo(0, 0));
  await expect(toggle).not.toHaveClass(/active/);
});

test.describe("map previews", () => {
  const card = (page, world) => page.locator(`.map-card[data-world="${world}"]`);

  // Stands in for squaremap: its settings list the given worlds, and its
  // page is a stub.
  async function fakeSquaremap(page, worlds) {
    await page.route("**/map/tiles/settings.json", (route) =>
      route.fulfill({ json: { worlds: worlds.map((name) => ({ name })) } }),
    );
    await page.route((url) => url.pathname === "/map/", (route) =>
      route.fulfill({ contentType: "text/html", body: "<!doctype html><title>map</title>" }),
    );
  }

  test("each world gets its own preview without squaremap's controls", async ({ page }) => {
    const worlds = ["minecraft_overworld", "minecraft_the_nether", "minecraft_the_end"];
    await fakeSquaremap(page, worlds);
    await open(page);
    for (const world of worlds) {
      // The frames load lazily, once they're near the window.
      await card(page, world).scrollIntoViewIfNeeded();
      await expect(card(page, world)).toHaveAttribute("data-ready", "true");
      const src = new URL(await card(page, world).locator("iframe").getAttribute("src"), "http://x");
      expect(src.pathname).toBe("/map/");
      expect(src.searchParams.get("world")).toBe(world);
      for (const p of ["show_controls", "show_sidebar", "show_coordinates", "show_link_button"]) {
        expect(src.searchParams.get(p), p).toBe("false");
      }
      await expect(card(page, world).locator("a.btn")).toHaveAttribute("href", `/map/?world=${world}`);
    }
  });

  test("a world squaremap hasn't rendered keeps its placeholder", async ({ page }) => {
    await fakeSquaremap(page, ["minecraft_overworld"]);
    await open(page);
    await card(page, "minecraft_the_nether").scrollIntoViewIfNeeded();
    await expect(card(page, "minecraft_overworld")).toHaveAttribute("data-ready", "true");
    await expect(card(page, "minecraft_the_nether")).toHaveAttribute("data-ready", "false");
    await expect(card(page, "minecraft_the_nether").locator("iframe")).not.toHaveAttribute("src");
    await expect(card(page, "minecraft_the_nether").locator(".map-placeholder")).toBeVisible();
  });

  test("no map at all shows every placeholder", async ({ page }) => {
    await open(page);
    await expect(page.locator(".map-placeholder:visible")).toHaveCount(3);
    await expect(page.locator(".map-card iframe[src]")).toHaveCount(0);
  });
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
        await page.waitForFunction(() => document.getElementById("sheet-skin").naturalWidth > 0);
        await page.screenshot({ path: `screenshots/${size}-${scheme}-sheet.png` });
      });
    }
  }
});

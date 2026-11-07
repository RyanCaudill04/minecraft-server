// Unit tests for sites/admin/status.js, with Node's built-in test runner:
//
//   node --test tests/admin/status.test.js

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import { actionHint, consoleLock, enabledActions, summary, unitMessage } from "../../sites/admin/status.js";

const owner = { email: "owner@example.com", control: true, console: true };
const status = (state, unit) => ({ state, online: 0, max: 20, players: [], ...(unit && { unit: { active: unit } }) });

describe("summary", () => {
  test("is loading before the first status", () => {
    assert.deepEqual(summary(null), { state: "loading", title: "Checking…", message: "One moment." });
  });

  // The Overview card used to say Online from the ping while the Server
  // card said Stopped from systemd. Both now show this.
  for (const [unit, ping, state, title] of [
    ["inactive", "offline", "offline", "Stopped"],
    ["failed", "offline", "offline", "Failed"],
    ["activating", "offline", "starting", "Starting…"],
    ["deactivating", "active", "starting", "Stopping…"],
  ]) {
    test(`trusts systemd while the unit is ${unit}`, () => {
      const got = summary(status(ping, unit));
      assert.equal(got.state, state);
      assert.equal(got.title, title);
    });
  }

  for (const [ping, title] of [
    ["active", "Online"],
    ["sleeping", "Sleeping"],
    ["starting", "Starting up"],
    ["offline", "Offline"],
  ]) {
    test(`trusts the ping (${ping}) while the unit runs`, () => {
      assert.equal(summary(status(ping, "active")).title, title);
    });
    test(`trusts the ping (${ping}) when systemd can't be read`, () => {
      const got = summary(status(ping));
      assert.equal(got.state, ping);
      assert.equal(got.title, title);
    });
  }

  test("names Paper's version while it's up", () => {
    assert.equal(summary({ ...status("active", "active"), version: "Paper 26.2" }).message, "Paper 26.2 is running.");
  });

  test("says admin is unreachable when the status fetch failed", () => {
    const got = summary({ state: "unknown", online: 0, max: 0, players: [] });
    assert.equal(got.state, "unknown");
    assert.equal(got.title, "Can't reach admin");
  });

  test("treats an unexpected ping state as unknown", () => {
    assert.equal(summary(status("exploded", "active")).state, "unknown");
  });
});

describe("unitMessage", () => {
  test("explains a running unit with Paper asleep", () => {
    assert.equal(unitMessage(status("sleeping", "active")), "LazyMC is up. Paper sleeps until someone joins.");
  });
  test("explains a running unit nobody answers for", () => {
    assert.match(unitMessage(status("offline", "active")), /isn't answering pings yet/);
  });
  // The hint under the buttons says systemd can't be read; this line
  // shouldn't repeat it.
  test("falls back to the ping when systemd can't be read", () => {
    assert.equal(unitMessage(status("sleeping")), summary(status("sleeping")).message);
  });
  test("matches the summary while systemd knows more", () => {
    const s = status("offline", "failed");
    assert.equal(unitMessage(s), summary(s).message);
  });
});

describe("enabledActions", () => {
  test("offers only Start while stopped", () => {
    assert.deepEqual(enabledActions(owner, status("offline", "inactive"), null), {
      start: true,
      stop: false,
      restart: false,
      wake: false,
    });
  });
  test("offers Stop, Restart and Wake while Paper sleeps", () => {
    assert.deepEqual(enabledActions(owner, status("sleeping", "active"), null), {
      start: false,
      stop: true,
      restart: true,
      wake: true,
    });
  });
  test("offers nothing while an action is in flight", () => {
    assert.ok(!Object.values(enabledActions(owner, status("active", "active"), "restart")).some(Boolean));
  });
  test("offers nothing without control", () => {
    const me = { ...owner, control: false };
    assert.ok(!Object.values(enabledActions(me, status("offline", "inactive"), null)).some(Boolean));
  });
});

describe("actionHint", () => {
  test("says nothing while an action is in flight", () => {
    assert.equal(actionHint(owner, status("active", "active"), "stop"), "");
  });
  test("passes on the lock reason for an unverified user", () => {
    const me = { control: false, console: false, locked: "Locked: the Access token didn't name a user." };
    assert.equal(actionHint(me, status("active", "active"), null), me.locked);
  });
  // A signed-in user without RCON gets locked = the RCON message, which
  // says nothing about the server controls.
  test("doesn't blame RCON when systemd is what's missing", () => {
    const me = { email: "owner@example.com", control: false, console: false, locked: "RCON isn't configured." };
    assert.match(actionHint(me, status("active"), null), /systemd/);
  });
  for (const [unit, want] of [
    ["active", /Start is off/],
    ["inactive", /Stop and Restart are off/],
    ["failed", /Stop and Restart are off/],
    ["activating", /started/],
    ["deactivating", /stopped/],
    [undefined, /can't read minecraft.service/],
  ]) {
    test(`explains the buttons while the unit is ${unit ?? "unreadable"}`, () => {
      assert.match(actionHint(owner, status("active", unit), null), want);
    });
  }
});

describe("consoleLock", () => {
  test("is open with RCON", () => {
    assert.equal(consoleLock(owner), "");
  });
  test("says why without RCON", () => {
    assert.equal(consoleLock({ ...owner, console: false, locked: "RCON isn't configured." }), "RCON isn't configured.");
  });
  test("waits for /api/me", () => {
    assert.equal(consoleLock(null), "Checking…");
  });
});

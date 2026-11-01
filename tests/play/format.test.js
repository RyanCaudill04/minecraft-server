// Unit tests for sites/play/format.js, with Node's built-in test runner.
// Number and date formats follow the default locale and time zone, so run
// them in en-US and UTC, as CI does:
//
//   LC_ALL=en_US.UTF-8 TZ=UTC node --test tests/play/format.test.js

import assert from "node:assert/strict";
import { describe, test } from "node:test";

import {
  accentHue,
  capitalize,
  fmtAgo,
  fmtDistance,
  fmtHours,
  fmtNum,
  fmtPlaytime,
  fmtWhen,
  mcClock,
  phase,
  weather,
} from "../../sites/play/format.js";

// One Minecraft hour is 1000 ticks; one real second is 20 ticks.
const MINUTE = 20 * 60;
const HOUR = 60 * MINUTE;

describe("mcClock", () => {
  for (const [tod, want] of [
    [0, "6:00 AM"], // tick 0 is sunrise
    [500, "6:30 AM"],
    [5999, "11:59 AM"],
    [6000, "12:00 PM"], // noon
    [7000, "1:00 PM"],
    [12000, "6:00 PM"],
    [17999, "11:59 PM"],
    [18000, "12:00 AM"], // midnight
    [19000, "1:00 AM"],
    [23999, "5:59 AM"],
  ]) {
    test(`${tod} is ${want}`, () => assert.equal(mcClock(tod), want));
  }
});

describe("phase", () => {
  for (const [tod, want] of [
    [0, "Sunrise"],
    [999, "Sunrise"],
    [1000, "Morning"],
    [4999, "Morning"],
    [5000, "Noon"],
    [6999, "Noon"],
    [7000, "Afternoon"],
    [11000, "Sunset"],
    [12999, "Sunset"],
    [13000, "Night"],
    [18000, "Night"],
    [22999, "Night"],
    [23000, "Dawn"],
    [23999, "Dawn"],
  ]) {
    test(`${tod} is ${want}`, () => assert.equal(phase(tod), want));
  }
});

describe("accentHue", () => {
  test("matches each keyframe exactly", () => {
    for (const [tod, hue] of [
      [0, 30],
      [1500, 205],
      [10500, 212],
      [12000, 18],
      [13000, 280],
      [14500, 238],
      [22000, 238],
      [23300, 280],
      [24000, 30],
    ]) {
      assert.equal(accentHue(tod), hue, `tick ${tod}`);
    }
  });

  test("interpolates without wrapping when that's shorter", () => {
    // 280 -> 238 goes down 42 degrees.
    assert.equal(accentHue(13750), 259);
    // Night holds still.
    assert.equal(accentHue(18000), 238);
  });

  test("wraps up through 360 from pre-dawn 280 to sunrise 30", () => {
    // The short way is +110 degrees through 0, not -250 through blue.
    assert.equal(accentHue(23650), 335);
    assert.equal(accentHue(23900), 14);
  });

  test("wraps up through 360 from afternoon 212 to sunset 18", () => {
    assert.equal(accentHue(11250), 295);
  });

  test("wraps down through 0 from sunset 18 to dusk 280", () => {
    assert.equal(accentHue(12500), 329);
  });

  test("always stays in 0-359", () => {
    for (let tod = 0; tod <= 24000; tod += 50) {
      const h = accentHue(tod);
      assert.ok(Number.isInteger(h) && h >= 0 && h < 360, `tick ${tod}: ${h}`);
    }
  });

  test("falls back to afternoon blue past the end of the day", () => {
    assert.equal(accentHue(24001), 212);
  });
});

describe("fmtNum", () => {
  for (const [n, want] of [
    [0, "0"],
    [undefined, "0"],
    [null, "0"],
    [-5, "-5"],
    [999, "999"],
    [1234, "1,234"],
    [99_999, "99,999"],
    [100_000, "100K"], // compact from 100k up
    [123_456, "123.5K"],
    [1_234_567, "1.2M"],
  ]) {
    test(`${n} is ${want}`, () => assert.equal(fmtNum(n), want));
  }
});

describe("fmtPlaytime", () => {
  for (const [ticks, want] of [
    [0, "0m"],
    [MINUTE - 1, "0m"],
    [MINUTE, "1m"],
    [HOUR - 1, "59m"],
    [HOUR, "1h 0m"],
    [HOUR + 15 * MINUTE, "1h 15m"],
    [100 * HOUR + 5 * MINUTE, "100h 5m"],
  ]) {
    test(`${ticks} ticks is ${want}`, () => assert.equal(fmtPlaytime(ticks), want));
  }
});

describe("fmtHours", () => {
  test("rounds down to whole hours", () => {
    assert.equal(fmtHours(0), "0 hours");
    assert.equal(fmtHours(2 * HOUR - 1), "1 hours");
    assert.equal(fmtHours(1500 * HOUR), "1,500 hours");
  });
});

describe("fmtDistance", () => {
  for (const [cm, want] of [
    [0, "0 m"],
    [49, "0 m"],
    [50, "1 m"],
    [75_000, "750 m"],
    [100_000, "1.0 km"], // km from 1 km up
    [250_000, "2.5 km"],
    [12_345_678, "123.5 km"],
  ]) {
    test(`${cm} cm is ${want}`, () => assert.equal(fmtDistance(cm), want));
  }
});

describe("fmtAgo", () => {
  const now = Date.parse("2026-10-27T12:00:00Z");
  const ago = (secs) => new Date(now - secs * 1000).toISOString();

  for (const [iso, want] of [
    [undefined, "never"],
    ["", "never"],
    [ago(0), "just now"],
    [ago(-3600), "just now"], // a clock ahead of ours
    [ago(59), "just now"],
    [ago(60), "1m ago"],
    [ago(3599), "59m ago"],
    [ago(3600), "1h ago"],
    [ago(86_399), "23h ago"],
    [ago(86_400), "1d ago"],
    [ago(30 * 86_400), "30d ago"],
  ]) {
    test(`${iso} is ${want}`, (t) => {
      t.mock.timers.enable({ apis: ["Date"], now });
      assert.equal(fmtAgo(iso), want);
    });
  }
});

describe("fmtWhen", () => {
  test("shows only the time for today", (t) => {
    t.mock.timers.enable({ apis: ["Date"], now: Date.parse("2026-10-27T12:00:00Z") });
    assert.match(fmtWhen("2026-10-27T09:05:00Z"), /^9:05\sAM$/);
  });

  test("adds the date for other days", (t) => {
    t.mock.timers.enable({ apis: ["Date"], now: Date.parse("2026-10-27T12:00:00Z") });
    assert.match(fmtWhen("2026-10-26T21:00:00Z"), /^Oct 26, 2026, 9:00\sPM$/);
  });
});

describe("weather", () => {
  test("thunder beats rain", () => {
    assert.equal(weather({ raining: true, thundering: true }), "Thunderstorm");
    assert.equal(weather({ raining: true, thundering: false }), "Rain");
    assert.equal(weather({}), "Clear");
  });
});

describe("capitalize", () => {
  test("uppercases the first letter only", () => {
    assert.equal(capitalize("easy"), "Easy");
    assert.equal(capitalize("hARD"), "HARD");
    assert.equal(capitalize(""), "");
    assert.equal(capitalize(), "");
  });
});

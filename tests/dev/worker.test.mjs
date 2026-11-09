// Unit tests for dev/worker.mjs, with a stand-in for the static assets
// binding. Run with: node --test tests/dev/worker.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import worker from "../../dev/worker.mjs";
import * as workerModule from "../../dev/worker.mjs";

// assets serves files from a map of path to body, and records what was
// asked for.
function assets(files) {
  const asked = [];
  return {
    asked,
    async fetch(req) {
      const path = new URL(req.url).pathname;
      asked.push(path);
      return path in files ? new Response(files[path]) : new Response("not found", { status: 404 });
    },
  };
}

const call = (env, path, init) => worker.fetch(new Request("https://dev-play.example" + path, init), env);

test("serves the snapshot for an API route, whatever the query", async () => {
  const env = { ASSETS: assets({ "/api/logs.json": '{"lines":["hi"]}' }) };
  const res = await call(env, "/api/logs?lines=200");
  assert.equal(res.status, 200);
  assert.equal(res.headers.get("Content-Type"), "application/json");
  assert.equal(res.headers.get("Cache-Control"), "no-store");
  assert.deepEqual(await res.json(), { lines: ["hi"] });
  assert.deepEqual(env.ASSETS.asked, ["/api/logs.json"]);
});

test("a reading's checked time is now, not the snapshot's", async () => {
  const env = { ASSETS: assets({ "/api/status.json": '{"state":"active","checked":"2020-01-01T00:00:00Z"}', "/api/players.json": "[]" }) };
  const before = Date.now();
  const got = await (await call(env, "/api/status")).json();
  assert.equal(got.state, "active");
  assert.ok(Date.parse(got.checked) >= before - 1000, got.checked);
  assert.deepEqual(await (await call(env, "/api/players")).json(), []);
});

test("a route without a snapshot is a JSON 404", async () => {
  const res = await call({ ASSETS: assets({}) }, "/api/nothing");
  assert.equal(res.status, 404);
  assert.ok((await res.json()).error);
});

test("refuses every change with a message the page shows", async () => {
  const env = { ASSETS: assets({ "/api/status.json": "{}" }) };
  for (const path of ["/api/console", "/api/server/stop", "/api/players/whitelist/add", "/api/backups/run"]) {
    const res = await call(env, path, { method: "POST", body: "{}" });
    assert.equal(res.status, 403, path);
    assert.deepEqual(await res.json(), { error: "This is the dev site: changes are turned off." }, path);
  }
  assert.deepEqual(env.ASSETS.asked, []);
});

test("only plain names reach the snapshot files", async () => {
  const env = { ASSETS: assets({ "/api/status.json": "{}", "/index.html": "<html>" }) };
  for (const path of ["/api/status.json", "/api/backups/world.tar.gz", "/api/..%2Findex", "/api/Status", "/api/"]) {
    const res = await call(env, path);
    assert.equal(res.status, 404, path);
  }
  assert.deepEqual(env.ASSETS.asked, []);
});

test("other methods are refused", async () => {
  const res = await call({ ASSETS: assets({}) }, "/api/status", { method: "DELETE" });
  assert.equal(res.status, 405);
});

test("everything outside /api/ is a static asset", async () => {
  const env = { ASSETS: assets({ "/shared/base.css": "body{}" }) };
  const res = await call(env, "/shared/base.css");
  assert.equal(res.status, 200);
  assert.equal(await res.text(), "body{}");
});

test("exports only the handler", () => {
  // Workers treat every named export as an entry point, and refuse to
  // start if one isn't a handler.
  assert.deepEqual(Object.keys(workerModule), ["default"]);
});

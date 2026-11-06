// The dev sites on Cloudflare: dev-play.ryancaudill.dev and
// dev-admin.ryancaudill.dev. Each serves its site's real page as static
// assets, and this Worker answers /api/* from the JSON snapshot that
// scripts/build-dev.sh saved next to it (the TestDevSnapshot tests in
// web/cmd/play and web/cmd/admin). All of it is synthetic.
//
// GET /api/<name> serves api/<name>.json, whatever the query string. Every
// change (any POST) is refused with a message the page shows, so nothing
// on the dev sites reaches the real server.

// Workers treat every named export as an entry point, so this stays
// private; tests/dev checks the message itself.
const DEV_REFUSAL = "This is the dev site: changes are turned off.";

function json(status, body) {
  return new Response(JSON.stringify(body) + "\n", {
    status,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (!url.pathname.startsWith("/api/")) {
      return env.ASSETS.fetch(request);
    }
    if (request.method === "POST") {
      return json(403, { error: DEV_REFUSAL });
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      return json(405, { error: "method not allowed" });
    }
    const name = url.pathname.slice("/api/".length);
    // One plain name only: no folders, dots or encoded characters, so a
    // request can only reach a snapshot file.
    if (!/^[a-z]+$/.test(name)) {
      return json(404, { error: "not in the dev snapshot" });
    }
    const asset = await env.ASSETS.fetch(new Request(new URL(`/api/${name}.json`, url)));
    if (!asset.ok) {
      return json(404, { error: "not in the dev snapshot" });
    }
    const body = await asset.json();
    // A status or health reading says when it was taken, and the pages
    // flag an old one as stale. The snapshot is days old by the time
    // anyone looks, so it reads as taken just now.
    if (body && typeof body === "object" && !Array.isArray(body) && typeof body.checked === "string") {
      body.checked = new Date().toISOString();
    }
    const res = json(200, body);
    return request.method === "HEAD" ? new Response(null, res) : res;
  },
};

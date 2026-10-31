// admin.ryancaudill.dev: polls the admin API and renders the dashboard.
// Updates only text and data-state, so polling never moves keyboard focus;
// the list of changes is rebuilt only when the release changes.

const STATUS_MS = 10_000; // server state and who's online
const DEPLOY_MS = 60_000; // the active release (changes a few times a day)

// Paper sends this UUID for players who hide themselves from the list.
const ANONYMOUS_ID = "00000000-0000-0000-0000-000000000000";

const STATES = {
  active: { title: "Online", message: (s) => `${s.version || "Paper"} is running.` },
  sleeping: { title: "Sleeping", message: () => "LazyMC is holding the port. A join wakes Paper." },
  starting: { title: "Starting up", message: () => "Paper is starting." },
  offline: { title: "Offline", message: () => "Neither Paper nor LazyMC answers a status ping." },
  unknown: { title: "Can't reach admin", message: () => "The admin API isn't answering. Retrying…" },
};

const KINDS = { feature: "Feature", fix: "Bug fix", other: "Other" };
const REPO = "https://github.com/RyanCaudill04/minecraft-server";

const data = { status: null, deploy: null };

const $ = (id) => document.getElementById(id);

// ---------- Fetching ----------

async function getJSON(path) {
  const res = await fetch(path, { cache: "no-store" });
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
  return res.json();
}

async function refreshStatus() {
  try {
    data.status = await getJSON("/api/status");
  } catch (err) {
    console.warn(err);
    data.status = { state: "unknown", online: 0, max: 0, players: [] };
  }
  renderStatus();
  tick();
}

async function refreshDeploy() {
  try {
    data.deploy = await getJSON("/api/deploy");
  } catch (err) {
    console.warn(err);
    data.deploy = null;
  }
  renderDeploy();
}

// ---------- Rendering ----------

function renderStatus() {
  const s = data.status;
  const info = STATES[s.state] ?? STATES.unknown;

  for (const node of document.querySelectorAll("[data-state]")) node.dataset.state = s.state;
  $("pill-text").textContent = info.title;
  $("state-title").textContent = info.title;
  $("state-message").textContent = info.message(s);

  $("online").textContent = s.online;
  $("max").textContent = s.max ? `/ ${s.max}` : "";
  // Paper's sample lists at most 12 players and leaves out hidden ones, so
  // the rest are "more", not necessarily hidden.
  const names = s.players.filter((p) => p.id !== ANONYMOUS_ID).map((p) => p.name);
  const more = s.online - names.length;
  let sub = names.join(", ");
  if (more > 0) sub += names.length ? ` and ${more} more` : `${more} online`;
  $("online-sub").textContent = s.state === "active" ? sub || "Nobody's on." : "Not running.";
}

function renderDeploy() {
  const d = data.deploy;
  const tag = d?.active;
  const text = d ? tag || "None" : "Unknown";
  $("release").textContent = text;
  $("deploy-release").textContent = text;

  let sub;
  if (!d) sub = "Couldn't read the deploy state.";
  else if (!tag) sub = "mc-agent hasn't installed a release yet.";
  // Changes are newest first, so this is the latest one.
  else sub = d.changes?.[0]?.title ?? "Installed by mc-agent.";
  $("release-sub").textContent = sub;
  $("release-sub").title = sub;

  setKind($("release-kind"), d?.kind);
  setKind($("deploy-kind"), d?.kind);
  setTime("deploy-released", d?.released_at);
  setTime("deploy-applied", d?.applied_at);

  // e.g. "4" over "1 feature · 1 fix · 2 other"
  const changes = d?.changes ?? [];
  $("deploy-count").textContent = d?.kind ? changes.length : "–";
  $("deploy-breakdown").textContent = Object.keys(KINDS)
    .map((k) => [k, changes.filter((c) => c.kind === k).length])
    .filter(([, n]) => n)
    .map(([k, n]) => `${n} ${k}`)
    .join(" · ");


  renderChanges(d);
}

// Shows a release's or change's kind as a badge, or hides it.
function setKind(node, kind) {
  node.hidden = !kind;
  node.dataset.kind = kind ?? "";
  node.textContent = KINDS[kind] ?? kind ?? "";
}

// Shows a time as "5m ago" in #id, and as a local date in #id-at.
function setTime(id, iso) {
  $(id).textContent = iso ? fmtAgo(iso) : "–";
  $(`${id}-at`).textContent = iso
    ? new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })
    : "";
}

let shownChanges;
function renderChanges(d) {
  let empty = "";
  if (!d) empty = "Couldn't read the deploy state.";
  else if (!d.active) empty = "No release is active.";
  else if (!d.kind) empty = "This release was built before release notes.";
  else if (!d.changes?.length) empty = "No changes since the last release.";
  $("deploy-changes-empty").textContent = empty;
  $("deploy-changes-empty").hidden = !empty;

  const key = JSON.stringify([d?.active, d?.changes]);
  if (key === shownChanges) return;
  shownChanges = key;

  const list = $("deploy-changes");
  list.replaceChildren(
    ...(d?.changes ?? []).map((c) => {
      const li = document.createElement("li");
      const kind = document.createElement("span");
      kind.className = "kind";
      setKind(kind, c.kind);
      const title = document.createElement("span");
      title.className = "title";
      title.textContent = c.title;
      li.append(kind, title);
      if (c.pr) {
        const a = document.createElement("a");
        a.href = `${REPO}/pull/${c.pr}`;
        a.textContent = `#${c.pr}`;
        li.append(a);
      }
      return li;
    }),
  );
}

// ---------- Scroll spy ----------

// Highlights the last section whose top has scrolled above a line 30% down
// the window, or the final one once the page is scrolled to the bottom.
const navLinks = [...document.querySelectorAll(".nav a")];
const sections = [...document.querySelectorAll(".section")];

function updateNav() {
  const line = innerHeight * 0.3;
  const atBottom = innerHeight + scrollY >= document.documentElement.scrollHeight - 2;
  let current = sections[0];
  for (const s of sections) if (s.getBoundingClientRect().top <= line) current = s;
  if (atBottom && scrollY > 0) current = sections.at(-1);
  if (scrollY < 8) current = sections[0];
  for (const a of navLinks) a.classList.toggle("active", a.hash === `#${current.id}`);
}
addEventListener("scroll", updateNav, { passive: true });
addEventListener("resize", updateNav);
updateNav();

// ---------- Startup ----------

function fmtAgo(iso) {
  const secs = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (secs < 60) return "just now";
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

function tick() {
  const checked = data.status?.checked;
  $("updated").textContent = checked ? `Updated ${fmtAgo(checked)}` : "Status unavailable";
}

let timers = [];
function start() {
  if (timers.length) return;
  refreshStatus();
  refreshDeploy();
  timers = [setInterval(refreshStatus, STATUS_MS), setInterval(refreshDeploy, DEPLOY_MS), setInterval(tick, 1000)];
}
function stop() {
  timers.forEach(clearInterval);
  timers = [];
}

// Don't poll while the tab is in the background.
document.addEventListener("visibilitychange", () => (document.hidden ? stop() : start()));
start();

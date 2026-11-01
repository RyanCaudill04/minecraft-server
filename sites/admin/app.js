// admin.ryancaudill.dev: polls the admin API and renders the dashboard.
// Updates only text and data-state, so polling never moves keyboard focus;
// the list of changes is rebuilt only when the release changes. The Server
// and Console sections POST actions and commands; their output is only
// ever set as text.

const STATUS_MS = 10_000; // server state and who's online
const DEPLOY_MS = 60_000; // the active release (changes a few times a day)
const PLUGINS_MS = 120_000; // installed plugins (change only on a restart)

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

const data = { status: null, deploy: null, plugins: null, me: null };

const PLUGIN_STATUS = {
  pinned: { label: "Pinned", note: "Matches the manifest." },
  changed: { label: "Changed", note: "Not the jar the manifest pins." },
  unpinned: { label: "Unpinned", note: "Not in the manifest." },
  invalid: { label: "Invalid", note: "" },
  missing: { label: "Missing", note: "In the manifest, not installed." },
};

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
  renderUnit();
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

async function refreshPlugins() {
  try {
    data.plugins = await getJSON("/api/plugins");
  } catch (err) {
    console.warn(err);
    data.plugins = null;
  }
  renderPlugins();
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

let shownPlugins;
function renderPlugins() {
  const d = data.plugins;
  let empty = "";
  if (!d) empty = "Couldn't read the plugins folder.";
  else if (!d.plugins?.length) empty = "No plugins installed.";
  else if (!d.manifest) empty = "plugins/manifest.json is missing, so nothing is pinned.";
  $("plugin-empty").textContent = empty;
  $("plugin-empty").hidden = !empty;

  const key = JSON.stringify(d);
  if (key === shownPlugins) return;
  shownPlugins = key;

  $("plugin-list").replaceChildren(
    ...(d?.plugins ?? []).map((p) => {
      const li = document.createElement("li");
      const who = document.createElement("span");
      who.className = "who";
      const name = document.createElement("span");
      name.className = "title";
      name.textContent = p.meta ? `${p.meta.name} ${p.meta.version}` : p.file;
      const file = document.createElement("span");
      file.className = "file";
      const info = PLUGIN_STATUS[p.status] ?? { label: p.status, note: "" };
      const hash = p.sha256 ? ` · sha256 ${p.sha256.slice(0, 12)}…` : "";
      file.textContent = `${p.file}${hash} · ${p.problem || info.note}`;
      file.title = p.sha256 ?? "";
      who.append(name, file);
      const kind = document.createElement("span");
      kind.className = "kind";
      kind.dataset.kind = p.status;
      kind.textContent = info.label;
      li.append(who, kind);
      return li;
    }),
  );
}

// ---------- Server controls ----------

// minecraft.service's systemd state. Paper's own state comes from the ping.
const UNIT = {
  active: {
    title: "Running",
    message: (s) =>
      s.state === "active"
        ? "LazyMC and Paper are up."
        : s.state === "starting"
          ? "LazyMC is up and Paper is starting."
          : s.state === "sleeping"
            ? "LazyMC is up. Paper sleeps until someone joins."
            : "systemd says it's running, but LazyMC isn't answering pings yet.",
  },
  activating: { title: "Starting…", message: () => "systemd is starting LazyMC." },
  deactivating: { title: "Stopping…", message: () => "Paper is saving the world. This can take a few minutes." },
  inactive: { title: "Stopped", message: () => "LazyMC and Paper are off. Nobody can join until it's started." },
  failed: { title: "Failed", message: () => "systemd gave up on it. Start it again, or check journalctl --user-unit minecraft." },
  unknown: { title: "Unknown", message: () => "admin can't read the unit's state from systemd." },
};

const ACTIONS = {
  start: { doing: "Starting the server…", done: "Asked systemd to start the server." },
  stop: { doing: "Stopping the server…", done: "Asked systemd to stop the server. Paper saves first." },
  restart: { doing: "Restarting the server…", done: "Asked systemd to restart the server. Paper saves first." },
  wake: { doing: "Waking Paper…", done: "LazyMC is starting Paper. The console works once it's up." },
};

const PENDING_MS = 2_000; // status poll while an action settles
const PENDING_FOR = 60_000; // how long to poll that fast

let pending = null; // the action being sent, if any
let fastUntil = 0;

// POSTs as JSON. The custom header is admin's CSRF check: another site
// can't send it without a CORS preflight, which admin never allows.
async function post(path, body) {
  const res = await fetch(path, {
    method: "POST",
    cache: "no-store",
    headers: { "Content-Type": "application/json", "X-Admin-Request": "1" },
    body: JSON.stringify(body ?? {}),
  });
  let reply = {};
  try {
    reply = await res.json();
  } catch {
    // Not JSON: fall back to the status code.
  }
  if (!res.ok) throw new Error(reply.error || `HTTP ${res.status}`);
  return reply;
}

async function refreshMe() {
  try {
    data.me = await getJSON("/api/me");
  } catch (err) {
    console.warn(err);
    data.me = { control: false, console: false, locked: "Couldn't ask admin who you are." };
  }
  renderMe();
  renderUnit();
  renderConsole();
}

function renderMe() {
  const me = data.me;
  $("me-email").textContent = me.email || "Not verified";
  $("me-sub").textContent = me.locked || "You can start, stop and restart the server and use its console.";
}

function renderUnit() {
  const s = data.status;
  const st = s?.unit?.active;
  const key = !s ? "loading" : st in UNIT ? st : "unknown";
  const info = UNIT[key] ?? { title: "Checking…", message: () => "One moment." };

  const card = document.querySelector(".unit-card");
  card.dataset.unit = key;
  $("unit-title").textContent = info.title;
  $("unit-message").textContent = info.message(s ?? {});

  renderChain(s, st);

  const can = data.me?.control && !pending;
  // Waking only makes sense while LazyMC holds the port for a sleeping
  // Paper; the button is there only then.
  const asleep = st === "active" && s?.state === "sleeping";
  const enabled = {
    start: can && (st === "inactive" || st === "failed"),
    stop: can && st === "active",
    restart: can && st === "active",
    wake: can && asleep,
  };
  for (const b of document.querySelectorAll("[data-action]")) {
    const off = !enabled[b.dataset.action];
    // Disabling the focused button would drop focus to the top of the
    // page, so keep it on its card instead.
    if (off && b === document.activeElement) b.closest(".card").focus();
    b.disabled = off;
  }
  card.querySelector('[data-action="wake"]').hidden = !asleep;
  $("console-asleep").hidden = !(asleep && data.me?.console);
}

// One step of the chain: on, busy, idle (Paper asleep), off or failed.
function setLink(id, link, text) {
  const li = $(id);
  li.dataset.link = link;
  li.querySelector(".link-state").textContent = text;
}

// systemd's unit, then LazyMC and Paper as the status ping sees them.
function renderChain(s, st) {
  const unit = {
    active: ["on", "Active"],
    activating: ["busy", "Activating"],
    deactivating: ["busy", "Deactivating"],
    inactive: ["off", "Inactive"],
    failed: ["failed", "Failed"],
  }[st] ?? ["unknown", s ? "Unknown" : "–"];
  setLink("chain-unit", ...unit);

  const ping = s?.state;
  const known = ping && ping !== "unknown";
  setLink(
    "chain-lazymc",
    ...(!known ? ["unknown", "–"] : ping === "offline" ? ["off", "Not answering"] : ["on", "Holding the port"]),
  );

  const players = s?.online === 1 ? "1 player" : `${s?.online ?? 0} players`;
  const paper = {
    active: ["on", `${s?.version || "Running"}, ${players}`],
    starting: ["busy", "Starting"],
    sleeping: ["idle", "Asleep until someone joins"],
    offline: ["off", "Off"],
  }[ping] ?? ["unknown", "–"];
  setLink("chain-paper", ...paper);
}

function setNote(text, isError = false) {
  const note = $("server-note");
  note.textContent = text;
  note.classList.toggle("error", isError);
}

// Asks before stopping or restarting, and says who would be kicked off.
function confirmAction(action) {
  if (action === "start" || action === "wake") return Promise.resolve(true);
  const s = data.status;
  const names = (s?.players ?? []).filter((p) => p.id !== ANONYMOUS_ID).map((p) => p.name);
  let who = "Nobody is online.";
  if (s?.online === 1) who = `${names[0] ?? "1 player"} is online and will be disconnected.`;
  else if (s?.online > 1) who = `${s.online} players are online and will be disconnected.`;

  const verb = action === "stop" ? "Stop" : "Restart";
  $("confirm-title").textContent = `${verb} the server?`;
  $("confirm-message").textContent =
    action === "stop"
      ? `${who} Nobody can join until it's started again.`
      : `${who} Paper saves and LazyMC starts again.`;
  $("confirm-ok").textContent = verb;

  const dialog = $("confirm");
  dialog.returnValue = "";
  dialog.showModal();
  return new Promise((resolve) =>
    dialog.addEventListener("close", () => resolve(dialog.returnValue === "ok"), { once: true }),
  );
}

async function runAction(action) {
  if (pending || !(await confirmAction(action))) return;
  pending = action;
  renderUnit();
  setNote(ACTIONS[action].doing);
  try {
    await post(`/api/server/${action}`);
    setNote(ACTIONS[action].done);
    fastUntil = Date.now() + PENDING_FOR;
  } catch (err) {
    setNote(err.message, true);
  } finally {
    pending = null;
  }
  await refreshStatus();
  if (fastUntil > Date.now()) pollFast();
}

// Polls status every couple of seconds for a minute after an action, so
// Stopping… and Stopped show up without waiting for the normal poll.
let fastTimer;
function pollFast() {
  clearTimeout(fastTimer);
  fastTimer = setTimeout(async () => {
    if (document.hidden || Date.now() > fastUntil) return;
    await refreshStatus();
    pollFast();
  }, PENDING_MS);
}

for (const b of document.querySelectorAll("[data-action]")) {
  b.addEventListener("click", () => runAction(b.dataset.action));
}

// ---------- Console ----------

const LOG_MS = 5_000;
const TRANSCRIPT_MAX = 20;

let consoleBusy = false; // a command is running

function renderConsole() {
  const on = !!data.me?.console;
  $("command").disabled = !on || consoleBusy;
  $("command-run").disabled = !on || consoleBusy;
  if (!on) {
    $("console-empty").textContent = data.me?.locked ?? "Checking…";
    $("log").textContent = data.me?.locked ?? "";
    $("log-updated").textContent = "";
  } else if (!$("transcript").children.length) {
    $("console-empty").textContent = "Output appears here.";
  }
}

// Adds one command and its output (or error) to the transcript, newest
// first. Everything goes in as text.
function addTranscript(cmd, text, isError) {
  const li = document.createElement("li");
  const head = document.createElement("div");
  head.className = "cmd";
  head.textContent = `/${cmd}`;
  const out = document.createElement("pre");
  out.className = isError ? "out error" : "out";
  out.textContent = text || "(no output)";
  li.append(head, out);
  const list = $("transcript");
  list.prepend(li);
  while (list.children.length > TRANSCRIPT_MAX) list.lastChild.remove();
  $("console-empty").hidden = true;
}

$("command-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const input = $("command");
  const cmd = input.value.trim().replace(/^\//, "");
  if (!cmd || consoleBusy) return;
  consoleBusy = true;
  renderConsole();
  try {
    const { output } = await post("/api/console", { command: cmd });
    addTranscript(cmd, output, false);
    input.value = "";
  } catch (err) {
    addTranscript(cmd, err.message, true);
  } finally {
    consoleBusy = false;
    renderConsole();
    input.focus();
    refreshLogs();
  }
});

async function refreshLogs() {
  if (!data.me?.console) return;
  let lines;
  try {
    ({ lines } = await getJSON("/api/logs?lines=200"));
  } catch (err) {
    console.warn(err);
    $("log-updated").textContent = "Couldn't read the log.";
    return;
  }
  const log = $("log");
  // Stay pinned to the newest line unless the reader has scrolled up.
  const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 24;
  const text = lines.length ? lines.join("\n") : "The log is empty. Paper writes it once it starts.";
  if (log.textContent !== text) log.textContent = text;
  if (atBottom) log.scrollTop = log.scrollHeight;
  $("log-updated").textContent = `Updated ${new Date().toLocaleTimeString([], { hour: "numeric", minute: "2-digit", second: "2-digit" })}`;
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
  refreshPlugins();
  refreshMe().then(refreshLogs);
  timers = [
    setInterval(refreshStatus, STATUS_MS),
    setInterval(refreshDeploy, DEPLOY_MS),
    setInterval(refreshPlugins, PLUGINS_MS),
    setInterval(refreshLogs, LOG_MS),
    setInterval(tick, 1000),
  ];
  if (fastUntil > Date.now()) pollFast();
}
function stop() {
  timers.forEach(clearInterval);
  timers = [];
  clearTimeout(fastTimer);
}

// Don't poll while the tab is in the background.
document.addEventListener("visibilitychange", () => (document.hidden ? stop() : start()));
start();

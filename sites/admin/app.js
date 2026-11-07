// admin.ryancaudill.dev: polls the admin API and renders the dashboard.
// Updates only text and data-state, so polling never moves keyboard focus;
// the list of changes is rebuilt only when the release changes. The Server
// and Console sections POST actions and commands; their output is only
// ever set as text. The Players section edits the whitelist and ops over
// RCON. The Backups section lists the world backups and keeps, deletes,
// downloads and starts them.

import { actionHint, consoleLock, enabledActions, summary, unitMessage } from "./status.js";

const STATUS_MS = 10_000; // server state and who's online
const DEPLOY_MS = 60_000; // the active release (changes a few times a day)
const PLUGINS_MS = 120_000; // installed plugins (change only on a restart)
const HEALTH_MS = 15_000; // the Pi's readings and Paper's TPS (cached 10s)

// Paper sends this UUID for players who hide themselves from the list.
const ANONYMOUS_ID = "00000000-0000-0000-0000-000000000000";

const KINDS = { feature: "Feature", fix: "Bug fix", other: "Other" };
const OUTCOMES = { success: "Succeeded", failed: "Failed", rolled_back: "Rolled back" };
const REPO = "https://github.com/RyanCaudill04/minecraft-server";

const data = { status: null, deploy: null, plugins: null, me: null, backups: null, players: null, health: null, pi: null };

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
  renderPlayers();
  if (data.pi) renderPi(); // the reboot buttons wait for the server to stop
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

async function refreshHealth() {
  try {
    data.health = await getJSON("/api/health");
  } catch (err) {
    console.warn(err);
    data.health = null;
  }
  renderHealth();
}

// ---------- Rendering ----------

// Long lists show their first few rows, with a Show all button after them,
// so a long list doesn't push the rest of the page down.
const LIST_SHOWN = 5;
const expanded = new Set(); // ids of the lists showing every row

// The first rows of items, or all of them once the list is expanded.
function shownRows(ul, items, shown = LIST_SHOWN) {
  return expanded.has(ul.id) ? items : items.slice(0, shown);
}

// Puts the Show all / Show fewer button after ul, and hides it when every
// row fits. rerender redraws the list after a toggle.
function showAll(ul, count, shown, rerender) {
  let more = $(`${ul.id}-more`);
  if (!more) {
    more = document.createElement("button");
    more.type = "button";
    more.id = `${ul.id}-more`;
    more.className = "show-all";
    more.setAttribute("aria-controls", ul.id);
    more.addEventListener("click", () => {
      if (!expanded.delete(ul.id)) expanded.add(ul.id);
      rerender();
    });
    ul.after(more);
  }
  const open = expanded.has(ul.id);
  more.hidden = count <= shown;
  more.setAttribute("aria-expanded", String(open));
  more.textContent = open ? "Show fewer" : `Show all ${count}`;
}

// The pill, the Overview card and the Server card all show summary(), so
// they agree; the Server card adds systemd's detail below it.
function renderStatus() {
  const s = data.status;
  const sum = summary(s);

  for (const node of document.querySelectorAll("[data-state]")) node.dataset.state = sum.state;
  $("pill-text").textContent = sum.title;
  $("state-title").textContent = sum.title;
  $("state-message").textContent = sum.message;

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
  // The card's link covers the clamped line, so its tooltip carries it.
  $("release-link").title = sub;

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

  renderLastDeploy(d);
  renderInstalled(d);
  renderChanges(d);
}

// The last deploy's outcome, e.g. "r8 Rolled back 5m ago", and its error.
function renderLastDeploy(d) {
  const r = d?.last_result;
  $("deploy-last-tag").textContent = r ? r.tag : d ? "None yet" : "Unknown";
  const outcome = $("deploy-last-outcome");
  outcome.hidden = !r;
  outcome.dataset.kind = r?.outcome ?? "";
  outcome.textContent = r ? (OUTCOMES[r.outcome] ?? r.outcome) : "";
  $("deploy-last-at").textContent = r ? fmtAgo(r.at) : "";
  $("deploy-last-at").title = r ? new Date(r.at).toLocaleString() : "";

  const error = r?.error ?? "";
  $("deploy-last-error").textContent = error;
  $("deploy-last-error").hidden = !error;
}

// Installed releases, newest first, marking the active and failed ones.
let shownInstalled;
function renderInstalled(d) {
  const key = JSON.stringify([d?.installed, d?.active, d?.failed]);
  if (key === shownInstalled) return;
  shownInstalled = key;

  const tags = d?.installed ?? [];
  const items = tags.map((tag) => {
    const li = document.createElement("li");
    const code = document.createElement("code");
    code.textContent = tag;
    li.append(code);
    const mark = tag === d.active ? "active" : tag === d.failed ? "failed" : "";
    if (mark) {
      const badge = document.createElement("span");
      badge.className = "kind";
      badge.dataset.kind = mark;
      badge.textContent = mark === "active" ? "Active" : "Failed";
      li.append(badge);
    }
    return li;
  });
  if (!items.length) {
    const li = document.createElement("li");
    li.className = "muted";
    li.textContent = d ? "None" : "Unknown";
    items.push(li);
  }
  $("deploy-installed").replaceChildren(...items);
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

  const list = $("deploy-changes");
  const key = JSON.stringify([d?.active, d?.changes, expanded.has(list.id)]);
  if (key === shownChanges) return;
  shownChanges = key;

  const changes = d?.changes ?? [];
  showAll(list, changes.length, LIST_SHOWN, () => renderChanges(data.deploy));
  list.replaceChildren(
    ...shownRows(list, changes).map((c) => {
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

  const ul = $("plugin-list");
  const key = JSON.stringify([d, expanded.has(ul.id)]);
  if (key === shownPlugins) return;
  shownPlugins = key;

  const plugins = d?.plugins ?? [];
  showAll(ul, plugins.length, LIST_SHOWN, renderPlugins);
  ul.replaceChildren(
    ...shownRows(ul, plugins).map((p) => {
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

// ---------- Pi health ----------

// When a reading turns amber or red. The Pi 4 starts throttling at 80 °C,
// and Paper can't hold 20 TPS once a tick takes over 50 ms.
const LIMITS = {
  temp: [70, 80], // °C, at or above
  memory: [85, 95], // % used, at or above
  disk: [15, 5], // % free, at or below
  load: [1, 2], // load per core, at or above
  tps: [18, 15], // at or below
  mspt: [40, 50], // ms, at or above
};

// "ok", "warn" or "bad" for v against [warn, bad]; low means lower is worse.
function level(v, [warn, bad], low = false) {
  if (low) return v <= bad ? "bad" : v <= warn ? "warn" : "ok";
  return v >= bad ? "bad" : v >= warn ? "warn" : "ok";
}
const WORST = { unknown: 0, idle: 0, ok: 1, warn: 2, bad: 3 };
const worst = (...ls) => ls.reduce((a, b) => (WORST[b] > WORST[a] ? b : a));

// Non-breaking spaces, so a narrow card wraps "/ 59.6 GB" onto its own
// line instead of splitting it.
const NBSP = "\u00a0";

// Sizes for Pi health and backups: "512 KB", "3.7 GB", "118 GB".
function fmtBytes(n) {
  if (n == null) return "–";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n < 100 && i ? n.toFixed(1) : Math.round(n)}${NBSP}${units[i]}`;
}
const fmtPct = (part, whole) => `${Math.round((100 * part) / whole)}%`;
const fixed = (n, digits = 2) => n.toFixed(digits);

function fmtUptime(secs) {
  const d = Math.floor(secs / 86400);
  const h = Math.floor((secs % 86400) / 3600);
  const m = Math.floor((secs % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}

// Sets one health card: its value, unit, sub line, level and bar.
function setHealth(id, { value = "–", unit = "", sub = "Not available.", lvl = "unknown", fill }) {
  const card = $(`health-${id}`);
  card.dataset.level = lvl;
  card.querySelector(".value").textContent = value;
  card.querySelector(".metric-unit").textContent = unit;
  card.querySelector(".metric-sub").textContent = sub;
  const bar = card.querySelector(".bar span");
  if (bar) bar.style.setProperty("--fill", fill == null ? "0%" : `${Math.min(100, Math.max(0, fill))}%`);
}

// Words for amber and red, so the color is never the only sign.
const WARN_WORDS = { warn: "Running low", bad: "Almost out" };

function renderHealth() {
  const h = data.health;
  $("health-updated").textContent = h ? "" : "Couldn't read the Pi's health.";
  renderPaperHealth(h?.paper);

  if (h?.temp_c != null) {
    const lvl = level(h.temp_c, LIMITS.temp);
    const words = { ok: "Normal.", warn: "Warm: the Pi slows down at 80 °C.", bad: "Hot: the Pi is slowing itself down." };
    setHealth("temp", { value: h.temp_c.toFixed(1), unit: "°C", sub: words[lvl], lvl });
  } else setHealth("temp", {});

  const m = h?.memory;
  if (m?.total) {
    const pct = (100 * m.used) / m.total;
    const lvl = level(pct, LIMITS.memory);
    const prefix = WARN_WORDS[lvl] ? `${WARN_WORDS[lvl]}: ` : "";
    setHealth("memory", {
      value: fmtBytes(m.used),
      unit: `/${NBSP}${fmtBytes(m.total)}`,
      sub: `${prefix}${fmtPct(m.used, m.total)} used, ${fmtBytes(m.available)} available.`,
      lvl,
      fill: pct,
    });
  } else setHealth("memory", {});

  const d = h?.disk;
  if (d?.total) {
    const freePct = (100 * d.free) / d.total;
    const lvl = level(freePct, LIMITS.disk, true);
    const prefix = WARN_WORDS[lvl] ? `${WARN_WORDS[lvl]}: ` : "";
    setHealth("disk", {
      value: fmtBytes(d.free),
      unit: `/${NBSP}${fmtBytes(d.total)}`,
      sub: `${prefix}${fmtPct(d.used, d.total)} used on the server's filesystem.`,
      lvl,
      fill: (100 * d.used) / d.total,
    });
  } else setHealth("disk", {});

  const l = h?.load;
  if (l) {
    const cpus = h.cpus || 1;
    const lvl = level(l.one / cpus, LIMITS.load);
    const busy = { ok: "", warn: "Busy. ", bad: "Overloaded. " }[lvl];
    setHealth("load", {
      value: fixed(l.one),
      unit: `/${NBSP}${cpus}${NBSP}${cpus === 1 ? "core" : "cores"}`,
      sub: `${busy}5 min ${fixed(l.five)}, 15 min ${fixed(l.fifteen)}.`,
      lvl,
    });
  } else setHealth("load", {});

  if (h?.uptime_seconds != null) {
    const since = new Date(Date.parse(h.checked) - h.uptime_seconds * 1000);
    setHealth("uptime", {
      value: fmtUptime(h.uptime_seconds),
      sub: `Up since ${since.toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}.`,
      lvl: "ok",
    });
  } else setHealth("uptime", {});
}

// Paper's TPS and MSPT, or why there aren't any. Asleep isn't a problem,
// so it gets a gray dot, not an error.
const PAPER_IDLE = {
  sleeping: ["Asleep", "TPS shows while Paper is awake. Reading it never wakes Paper."],
  starting: ["Starting", "TPS shows once Paper is up."],
  offline: ["Off", "Paper isn't running."],
};

function renderPaperHealth(p) {
  if (!p) return setHealth("paper", {});
  const idle = PAPER_IDLE[p.state];
  if (idle) return setHealth("paper", { value: idle[0], sub: idle[1], lvl: "idle" });
  if (p.state !== "active" || !p.tps) {
    return setHealth("paper", { value: p.state === "active" ? "Awake" : "–", sub: p.note || "Not available." });
  }

  const t = p.tps;
  let lvl = level(t.one, LIMITS.tps, true);
  let sub = `5 min ${fixed(t.five, 1)}, 15 min ${fixed(t.fifteen, 1)}.`;
  if (p.mspt) {
    lvl = worst(lvl, level(p.mspt.avg, LIMITS.mspt));
    const over = p.mspt.window ? ` over ${p.mspt.window}` : "";
    sub += ` ${fixed(p.mspt.avg, 1)} ms a tick${over} (max ${fixed(p.mspt.max, 1)}).`;
  }
  if (lvl !== "ok") sub = `${lvl === "bad" ? "Lagging" : "Slowing"}: ${sub}`;
  setHealth("paper", { value: fixed(t.one, 1), unit: "TPS", sub, lvl });
}

// ---------- Server controls ----------

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
  renderBackups();
  renderPlayers();
}

function renderMe() {
  $("me-email").textContent = data.me.email || "Not verified";
}

function renderUnit() {
  const s = data.status;
  const st = s?.unit?.active;

  const card = document.querySelector(".unit-card");
  $("unit-title").textContent = summary(s).title;
  $("unit-message").textContent = unitMessage(s);
  $("actions-hint").textContent = actionHint(data.me, s, pending);

  renderChain(s, st);

  // The wake button is there only while Paper sleeps behind LazyMC.
  const asleep = st === "active" && s?.state === "sleeping";
  const enabled = enabledActions(data.me, s, pending);
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
  return ask(
    `${verb} the server?`,
    action === "stop"
      ? `${who} Nobody can join until it's started again.`
      : `${who} Paper saves and LazyMC starts again.`,
    verb,
  );
}

// Shows the confirmation dialog and resolves true if its OK button was
// chosen.
function ask(title, message, ok) {
  $("confirm-title").textContent = title;
  $("confirm-message").textContent = message;
  $("confirm-ok").textContent = ok;

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

// The lock reason shows once, where the output would be. The log only
// needs sign-in, not RCON, so it stays up whenever admin knows who you are.
function renderConsole() {
  const locked = consoleLock(data.me);
  $("command").disabled = !!locked || consoleBusy;
  $("command-run").disabled = !!locked || consoleBusy;
  if (locked) $("console-empty").textContent = locked;
  else if (!$("transcript").children.length) $("console-empty").textContent = "Output appears here.";

  if (data.me && !data.me.email) {
    $("log").textContent = "Sign in to read the log.";
    $("log-updated").textContent = "";
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
  if (!data.me?.email) return;
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
  updateLogExpand();
  $("log-updated").textContent = `Updated ${new Date().toLocaleTimeString([], { hour: "numeric", minute: "2-digit", second: "2-digit" })}`;
}

// The log starts short. Expand shows up once there's more than fits, and
// stays while it's expanded so it can be collapsed again.
function updateLogExpand() {
  const log = $("log");
  const open = log.classList.contains("expanded");
  const button = $("log-expand");
  button.hidden = !open && log.scrollHeight <= log.clientHeight;
  button.setAttribute("aria-expanded", String(open));
  button.textContent = open ? "Collapse" : "Expand";
}

$("log-expand").addEventListener("click", () => {
  const log = $("log");
  log.classList.toggle("expanded");
  log.scrollTop = log.scrollHeight; // newest lines in view either way
  updateLogExpand();
});

// ---------- Players ----------

const PLAYERS_MS = 30_000; // the lists (Paper saves them on each change)

// A Minecraft name. admin checks it again before anything reaches RCON.
const PLAYER_NAME = /^[A-Za-z0-9_]{3,16}$/;

const PLAYER_LISTS = {
  whitelist: {
    add: (n) => ({ doing: `Adding ${n}…` }),
    on: (missing) => ({
      doing: "Turning the whitelist on…",
      ask: [
        "Turn the whitelist on?",
        missing.length
          ? `Only players on the list can join. ${joinNames(missing)} ${missing.length === 1 ? "has played here but isn't" : "have played here but aren't"} on it, so they'll be locked out. Cancel and add them first to keep them.`
          : "Only players on the list can join. Everyone who has played here is on it.",
        "Turn on",
      ],
    }),
    off: () => ({
      doing: "Turning the whitelist off…",
      ask: ["Turn the whitelist off?", "Anyone who knows the server's address can join.", "Turn off"],
    }),
    "add-all": (missing) => ({
      doing: `Adding ${missing.length === 1 ? missing[0].name : `${missing.length} players`}…`,
      ask: [`Whitelist ${missing.length === 1 ? missing[0].name : `all ${missing.length} players`}?`, `Adds ${joinNames(missing)} to the whitelist.`, "Add all"],
    }),
    remove: (n) => ({
      doing: `Removing ${n}…`,
      ask: [`Remove ${n} from the whitelist?`, `${n} can't join while the whitelist is on. Players already online stay until they leave.`, "Remove"],
    }),
  },
  ops: {
    add: (n) => ({
      doing: `Making ${n} an operator…`,
      ask: [`Make ${n} an operator?`, `${n} gets every command, including stop, op and whitelist.`, "Op"],
    }),
    remove: (n) => ({
      doing: `Removing ${n}'s operator status…`,
      ask: [`Deop ${n}?`, `${n} can no longer run operator commands.`, "Deop"],
    }),
  },
};

// "Alice", "Alice and Bob", "Alice, Bob and 3 others".
function joinNames(players) {
  const names = players.map((p) => p.name);
  if (names.length <= 1) return names.join("");
  if (names.length <= 3) return `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
  return `${names.slice(0, 2).join(", ")} and ${names.length - 2} others`;
}

// A change in flight. The buttons stay enabled meanwhile, so focus stays
// put; a second click is ignored.
let playersBusy = false;
let playersTried = false; // the lists have been asked for once

async function refreshPlayers() {
  playersTried = true;
  try {
    data.players = await getJSON("/api/players");
  } catch (err) {
    console.warn(err);
    data.players = null;
  }
  renderPlayers();
}

function setPlayersNote(text, isError = false) {
  const note = $("players-note");
  note.textContent = text;
  note.classList.toggle("error", isError);
}

// Changes need RCON, so Paper has to be awake. The lists still show from
// the files while it sleeps.
function playersLocked() {
  const me = data.me;
  if (!me) return "Checking…";
  if (!me.console) return me.locked ?? "RCON isn't available.";
  const s = data.status;
  switch (s?.state) {
    case "active":
      return "";
    case "sleeping":
      return "Paper is asleep, so the lists can't change: Paper looks up each player and saves the files itself. Wake it to edit them; LazyMC puts it back to sleep after 5 minutes with nobody online.";
    case "starting":
      return "Paper is starting. The lists can change once it's up.";
    case "offline":
      return "The server is off. Start it from the Server section to change the lists.";
  }
  return "Checking the server…";
}

function renderPlayers() {
  const d = data.players;
  const locked = playersLocked();
  const can = !locked;

  // The note only explains why editing is off once admin knows who you
  // are; a locked user sees why in the empty-list text instead.
  const why = data.me?.console && locked && data.status ? locked : "";
  $("players-asleep").hidden = !why;
  $("players-asleep-text").textContent = why;
  $("players-asleep").querySelector('[data-action="wake"]').hidden = data.status?.state !== "sleeping";

  const on = d?.white_list;
  const sw = $("whitelist-switch");
  sw.setAttribute("aria-checked", on ? "true" : "false");
  if (!can && sw === document.activeElement) sw.closest(".card").focus();
  sw.disabled = !can || on == null;
  $("whitelist-switch-state").textContent = !d ? "…" : on == null ? "unknown" : on ? "on" : "off";
  $("whitelist-mode-text").textContent = !d
    ? ""
    : on == null
      ? "server.properties has no white-list setting yet; Paper writes it on its first start."
      : on
        ? d.enforce_whitelist
          ? "Only players on the list can join, and anyone removed is kicked straight away."
          : "Only players on the list can join. Anyone removed stays until they leave."
        : "Anyone can join. The list is kept for when it's on.";
  const n = d?.whitelist.length ?? 0;
  $("whitelist-count").textContent = d ? (n === 1 ? "1 player" : `${n} players`) : "";

  const missing = d?.not_whitelisted ?? [];
  $("not-listed").hidden = !missing.length;
  $("not-listed-text").textContent = on
    ? `${missing.length === 1 ? "This player has" : "These players have"} joined before but can't now, because the whitelist is on.`
    : `${missing.length === 1 ? "This player has" : "These players have"} joined before. Add them before turning the whitelist on, or they'll be locked out.`;
  const all = $("whitelist-add-all");
  all.textContent = missing.length > 1 ? `Add all ${missing.length}` : "Add";
  if (!can && all === document.activeElement) all.closest(".card").focus();
  all.disabled = !can;

  for (const form of document.querySelectorAll(".player-form")) {
    for (const el of form.elements) {
      // Disabling the focused field would drop focus to the top of the
      // page, so keep it on the card instead.
      if (!can && el === document.activeElement) form.closest(".card").focus();
      el.disabled = !can;
    }
  }

  let unread = "";
  if (!d) unread = !playersTried ? "Loading…" : data.me && !data.me.email ? data.me.locked : "Couldn't read the lists.";
  setPlayerEmpty("whitelist-empty", unread || (d.whitelist.length ? "" : "Nobody is whitelisted."));
  setPlayerEmpty("ops-empty", unread || (d.ops.length ? "" : "Nobody is an operator."));

  renderPlayerList("whitelist", d?.whitelist ?? [], can);
  renderPlayerList("ops", d?.ops ?? [], can);
  renderPlayerList("not-listed", missing, can);
}

function setPlayerEmpty(id, text) {
  $(id).textContent = text;
  $(id).hidden = !text;
}

// Rebuilds a list when it changes, and puts focus back on the same
// button afterwards (or on the card, if that player is gone).
const shownPlayers = {};
// Player lists show this many players until Show all is pressed.
const PLAYERS_SHOWN = 3;

function renderPlayerList(list, players, can) {
  const ul = $(`${list}-list`);
  const open = expanded.has(ul.id);
  const key = JSON.stringify([players, can, open]);
  if (key === shownPlayers[list]) return;
  shownPlayers[list] = key;

  const active = document.activeElement;
  const refocus = ul.contains(active) ? active.dataset.name : null;

  const [label, aria, target, action] = {
    ops: ["Deop", (n) => `Deop ${n}`, "ops", "remove"],
    whitelist: ["Remove", (n) => `Remove ${n} from the whitelist`, "whitelist", "remove"],
    "not-listed": ["Add", (n) => `Add ${n} to the whitelist`, "whitelist", "add"],
  }[list];

  ul.replaceChildren(
    ...players.slice(0, open ? players.length : PLAYERS_SHOWN).map((p) => {
      const li = document.createElement("li");
      li.className = "player-tile";
      const who = document.createElement("span");
      who.className = "who";
      const name = document.createElement("span");
      name.className = "name";
      name.textContent = p.name;
      who.append(name);
      if (list === "ops") {
        const meta = document.createElement("span");
        meta.className = "meta";
        meta.textContent = `Level ${p.level}`;
        meta.title = "Operator permission level, 1 to 4. Level 4 can run every command.";
        who.append(meta);
      }
      const b = button(label, action, p.name, "btn-secondary");
      b.setAttribute("aria-label", aria(p.name));
      b.disabled = !can;
      b.addEventListener("click", () => changePlayer(target, action, p.name));
      li.append(playerHead(p), who, b);
      return li;
    }),
  );
  showAll(ul, players.length, PLAYERS_SHOWN, renderPlayers);

  if (refocus) {
    const again = [...ul.querySelectorAll("[data-name]")].find((n) => n.dataset.name === refocus && !n.disabled);
    (again ?? ul.closest(".card")).focus();
  }
}

// A player's skin head from mc-heads, as on play, or a blank tile if it
// doesn't load.
function playerHead(p) {
  const blank = () => {
    const span = document.createElement("span");
    span.className = "head-fallback";
    return span;
  };
  if (!p.uuid || p.uuid === ANONYMOUS_ID) return blank();
  const img = document.createElement("img");
  img.src = `https://mc-heads.net/avatar/${encodeURIComponent(p.uuid)}/72`;
  img.width = img.height = 36;
  img.alt = "";
  img.loading = "lazy";
  img.onerror = () => img.replaceWith(blank());
  return img;
}

async function changePlayer(list, action, name) {
  if (playersBusy) return false;
  if (!PLAYER_NAME.test(name)) {
    setPlayersNote("A Minecraft name is 3 to 16 letters, digits or underscores.", true);
    return false;
  }
  return playerRequest(list, action, PLAYER_LISTS[list][action](name), { name });
}

// Whitelist on, off and add-all: no name, and the server works out who
// is missing itself.
function whitelistAction(action) {
  if (playersBusy) return false;
  return playerRequest("whitelist", action, PLAYER_LISTS.whitelist[action](data.players?.not_whitelisted ?? []), {});
}

async function playerRequest(list, action, step, body) {
  if (step.ask && !(await ask(...step.ask))) return false;
  if (playersBusy) return false;

  playersBusy = true;
  renderPlayers();
  setPlayersNote(step.doing);
  let ok = false;
  try {
    const { output } = await post(`/api/players/${list}/${action}`, body);
    // Paper's own words, e.g. "Added steve to the whitelist" or "That
    // player does not exist".
    setPlayersNote(output || "Done.");
    ok = true;
  } catch (err) {
    setPlayersNote(err.message, true);
  } finally {
    playersBusy = false;
  }
  await refreshPlayers();
  return ok;
}

$("whitelist-switch").addEventListener("click", () => whitelistAction(data.players?.white_list ? "off" : "on"));
$("whitelist-add-all").addEventListener("click", () => whitelistAction("add-all"));

for (const form of document.querySelectorAll(".player-form")) {
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const input = form.elements.name;
    const name = input.value.trim();
    if (!name) return;
    if (await changePlayer(form.dataset.list, "add", name)) input.value = "";
    if (!input.disabled) input.focus();
  });
}

// ---------- Backups ----------

const BACKUPS_MS = 30_000; // the list (a new one appears nightly)
const BACKUP_RUN_MS = 3_000; // while a backup runs
const DAY_MS = 86_400_000;

let backupBusy = false; // a backup request is in flight
let backupTimer;

async function refreshBackups() {
  try {
    data.backups = await getJSON("/api/backups");
  } catch (err) {
    console.warn(err);
    data.backups = null;
  }
  renderBackups();
  // Poll faster while a backup runs, so it shows up when it's done.
  clearTimeout(backupTimer);
  if (backupRunning() && !document.hidden) backupTimer = setTimeout(refreshBackups, BACKUP_RUN_MS);
}

const backupRunning = () => data.backups?.unit?.active === "activating";

function setBackupNote(text, isError = false) {
  const note = $("backup-note");
  note.textContent = text;
  note.classList.toggle("error", isError);
}

const fmtDate = (iso) =>
  new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });

// "in 3 days", "tomorrow", or "at the next cleanup" once it's due.
function fmtUntil(iso) {
  const ms = new Date(iso) - Date.now();
  if (ms <= 0) return "at the next cleanup";
  const days = Math.ceil(ms / DAY_MS);
  if (days === 1) return ms < 12 * 3_600_000 ? `in ${Math.ceil(ms / 3_600_000)}h` : "within a day";
  return `in ${days} days`;
}

// What mc-backup.service is doing, for the run cell.
const BACKUP_UNIT = {
  activating: ["busy", "Backing up now…"],
  failed: ["failed", "The last run failed. See journalctl --user-unit mc-backup."],
  inactive: ["on", "Runs nightly at about 04:30."],
};

function renderBackups() {
  const d = data.backups;
  const can = !!data.me?.backups;
  const list = d?.backups ?? [];

  if (d) $("backup-policy").textContent = `World backups from mc-backup. Each one is removed after ${d.keep_days} days unless it's kept for safe keeping.`;
  $("backup-count").textContent = d ? list.length : "–";
  const kept = list.filter((b) => b.keep).length;
  $("backup-total").textContent = d ? `${fmtBytes(d.total_size)}${kept ? ` · ${kept} kept` : ""}` : "";
  $("backup-newest").textContent = list[0] ? fmtAgo(list[0].created) : d ? "None yet" : "–";
  $("backup-newest-at").textContent = list[0] ? fmtDate(list[0].created) : "";
  $("backup-free").textContent = d?.free != null ? fmtBytes(d.free) : "–";

  const [run, runText] = BACKUP_UNIT[d?.unit?.active] ?? ["unknown", d ? "Can't read mc-backup.service." : "Couldn't read the backups."];
  document.querySelector(".backup-run").dataset.run = run;
  $("backup-run-state").textContent = runText;
  const runBtn = $("backup-run");
  const runOff = !can || backupBusy || !d?.unit || backupRunning();
  if (runOff && runBtn === document.activeElement) runBtn.closest(".card").focus();
  runBtn.disabled = runOff;

  let empty = "";
  if (!d) empty = "Couldn't read the backup folder.";
  else if (!list.length) empty = "No backups yet. The first one runs tonight, or back up now.";
  else if (!can) empty = data.me?.locked ?? "";
  $("backup-empty").textContent = empty;
  $("backup-empty").hidden = !empty;

  renderBackupList(list, can);
}

// Rebuilds the list when it changes (or every hour, for the "removed in"
// text), and puts focus back on the same button afterwards.
let shownBackups;
function renderBackupList(list, can) {
  const ul = $("backup-list");
  const key = JSON.stringify([list, can, expanded.has(ul.id), Math.floor(Date.now() / 3_600_000)]);
  if (key === shownBackups) return;
  shownBackups = key;

  const active = document.activeElement;
  const refocus = ul.contains(active) ? [active.dataset.name, active.dataset.op] : null;

  showAll(ul, list.length, LIST_SHOWN, renderBackups);
  ul.replaceChildren(
    ...shownRows(ul, list).map((b) => {
      const li = document.createElement("li");
      const who = document.createElement("span");
      who.className = "who";
      const title = document.createElement("span");
      title.className = "title";
      title.textContent = fmtDate(b.created);
      const file = document.createElement("span");
      file.className = "file";
      file.textContent = `${b.name} · ${fmtBytes(b.size)} · ${fmtAgo(b.created)}${b.checksum ? "" : " · no checksum"}`;
      who.append(title, file);

      const badge = document.createElement("span");
      badge.className = "kind";
      if (b.keep) {
        badge.dataset.kind = "kept";
        badge.textContent = "Kept";
      } else if (!b.expires) {
        // Not a nightly backup (e.g. pre-migrate): the cleanup leaves it.
        badge.textContent = "Not auto-removed";
        badge.title = "Only the nightly backups are cleaned up. Delete this one when you no longer need it.";
      } else {
        const soon = new Date(b.expires) - Date.now() < 2 * DAY_MS;
        badge.dataset.kind = soon ? "expiring" : "";
        badge.textContent = `Removed ${fmtUntil(b.expires)}`;
        badge.title = `The nightly cleanup removes it after ${fmtDate(b.expires)}.`;
      }

      const actions = document.createElement("span");
      actions.className = "actions";
      const keep = button("Keep", "keep", b.name, "btn-secondary");
      keep.setAttribute("aria-pressed", String(b.keep));
      keep.setAttribute("aria-label", `Keep the ${fmtDate(b.created)} backup for safe keeping`);
      keep.disabled = !can;
      keep.addEventListener("click", () => setKeep(b, !b.keep));

      const download = document.createElement("a");
      download.className = "btn btn-secondary";
      download.textContent = "Download";
      download.dataset.name = b.name;
      download.dataset.op = "download";
      download.setAttribute("aria-label", `Download the ${fmtDate(b.created)} backup`);
      if (can) {
        download.href = `/api/backups/${encodeURIComponent(b.name)}`;
        download.download = b.name;
      } else {
        download.setAttribute("aria-disabled", "true");
      }

      const del = button("Delete", "delete", b.name, "btn-danger");
      del.setAttribute("aria-label", `Delete the ${fmtDate(b.created)} backup`);
      del.disabled = !can || b.keep;
      if (b.keep) del.title = "Kept backups can't be deleted. Remove the flag first.";
      del.addEventListener("click", () => deleteBackup(b));

      actions.append(keep, download, del);
      li.append(who, badge, actions);
      return li;
    }),
  );

  if (refocus) {
    const again = [...$("backup-list").querySelectorAll("[data-name]")].find(
      (n) => n.dataset.name === refocus[0] && n.dataset.op === refocus[1] && !n.disabled,
    );
    (again ?? document.querySelector(".backups-card")).focus();
  }
}

function button(text, op, name, kind) {
  const b = document.createElement("button");
  b.type = "button";
  b.className = `btn ${kind}`;
  b.textContent = text;
  b.dataset.name = name;
  b.dataset.op = op;
  return b;
}

// Runs one backup request, then refreshes the list. The row buttons stay
// enabled meanwhile, so focus stays put; a second click is ignored.
async function backupRequest(doing, path, body, done) {
  if (backupBusy) return;
  backupBusy = true;
  renderBackups();
  setBackupNote(doing);
  try {
    await post(path, body);
    setBackupNote(done);
  } catch (err) {
    setBackupNote(err.message, true);
  } finally {
    backupBusy = false;
  }
  await refreshBackups();
}

function setKeep(b, keep) {
  const when = fmtDate(b.created);
  backupRequest(
    keep ? "Keeping it…" : "Removing the flag…",
    `/api/backups/${encodeURIComponent(b.name)}/keep`,
    { keep },
    keep
      ? `Kept the ${when} backup. The cleanup won't remove it.`
      : `The ${when} backup is no longer kept, so the cleanup removes it once it's ${data.backups?.keep_days ?? 10} days old.`,
  );
}

async function deleteBackup(b) {
  const when = fmtDate(b.created);
  if (!(await ask("Delete this backup?", `The ${when} backup (${fmtBytes(b.size)}) is removed for good.`, "Delete"))) return;
  backupRequest("Deleting…", `/api/backups/${encodeURIComponent(b.name)}/delete`, null, `Deleted the ${when} backup.`);
}

$("backup-run").addEventListener("click", () =>
  backupRequest("Starting a backup…", "/api/backups/run", null, "Backup started. It shows up here when it's done, usually within a few minutes."),
);

// ---------- Scroll spy ----------

// ---------- Pi settings ----------

const PI_MS = 15_000; // the trial's state changes over a few minutes
const REBOOT_WORD = "reboot"; // admin checks the same word

let piBusy = false; // a request is in flight
let piDirty = false; // the form has edits that aren't saved yet

async function refreshPi() {
  try {
    data.pi = await getJSON("/api/pi");
  } catch (err) {
    console.warn(err);
    data.pi = null;
  }
  renderPi();
}

const fmtMilli = (m) => `${+(m / 1000).toFixed(1)}`;

// A short description of a set of settings, for the Saved and Waiting
// lines and the trial's banner.
function describePi(s) {
  if (!s) return "Nothing";
  const parts = [];
  if (s.arm_freq != null) parts.push(`${s.arm_freq} MHz`);
  if (s.over_voltage_delta != null) parts.push(`+${s.over_voltage_delta / 1000} mV`);
  if (s.temp_limit != null) parts.push(`limit ${s.temp_limit} °C`);
  if (s.fan) parts.push(`fan on from ${fmtMilli(s.fan[0].temp)} °C`);
  return parts.length ? parts.join(", ") : "Firmware defaults";
}

const PI_TRIAL = {
  rebooting: { title: "Rebooting", level: "busy" },
  checking: { title: "Checking the trial boot", level: "busy" },
  passed: { title: "Trial passed", level: "ok" },
  failed: { title: "Trial failed", level: "bad" },
  kept: { title: "Kept", level: "ok" },
  discarded: { title: "Going back", level: "busy" },
  expired: { title: "Trial not kept", level: "warn" },
};

const FAN_STEPS = 4;
const fanCell = (i, part) => $(`pi-fan-${i}-${part}`);

// The fan curve's rows, built once.
function buildFanRows() {
  const body = $("pi-fan-rows");
  for (let i = 0; i < FAN_STEPS; i++) {
    const tr = document.createElement("tr");
    const th = document.createElement("th");
    th.scope = "row";
    th.textContent = String(i + 1);
    tr.append(th);
    for (const [part, step, label] of [
      ["temp", "0.5", "on at, °C"],
      ["off", "0.5", "off below, °C"],
      ["speed", "1", "speed, 0 to 255"],
    ]) {
      const td = document.createElement("td");
      const input = document.createElement("input");
      input.type = "number";
      input.inputMode = "decimal";
      input.step = step;
      input.id = `pi-fan-${i}-${part}`;
      input.disabled = true;
      input.setAttribute("aria-label", `Step ${i + 1} ${label}`);
      td.append(input);
      tr.append(td);
    }
    body.append(tr);
  }
}
buildFanRows();

function fillPiForm(s, limits) {
  $("pi-arm-freq").value = s?.arm_freq ?? "";
  $("pi-volts").value = s?.over_voltage_delta != null ? s.over_voltage_delta / 1000 : "";
  $("pi-temp-limit").value = s?.temp_limit ?? "";
  $("pi-fan-custom").checked = !!s?.fan;
  const fan = s?.fan ?? limits.fan_default;
  fan.forEach((st, i) => {
    fanCell(i, "temp").value = fmtMilli(st.temp);
    fanCell(i, "off").value = fmtMilli(st.temp - st.hyst);
    fanCell(i, "speed").value = st.speed;
  });
}

// One number from the form, scaled to what config.txt takes, or an
// error in the units the page shows. An empty box is undefined: the
// firmware's default.
function piNumber(id, label, range, scale = 1, unit = "") {
  const text = $(id).value.trim();
  if (text === "") return { value: undefined };
  const n = Number(text);
  const raw = Math.round(n * scale);
  if (!Number.isFinite(n) || raw < range.min || raw > range.max) {
    return { error: `${label} must be ${range.min / scale} to ${range.max / scale}${unit}.` };
  }
  return { value: raw };
}

// Reads the form into the settings admin takes, or says what's wrong.
function readPiForm(limits) {
  const s = {};
  for (const [key, id, label, scale, unit] of [
    ["arm_freq", "pi-arm-freq", "The top clock", 1, " MHz"],
    ["over_voltage_delta", "pi-volts", "The extra voltage", 1000, " mV"],
    ["temp_limit", "pi-temp-limit", "The temperature limit", 1, " °C"],
  ]) {
    const { value, error } = piNumber(id, label, limits[key], scale, unit);
    if (error) return { error };
    if (value !== undefined) s[key] = value;
  }
  if ($("pi-fan-custom").checked) {
    s.fan = [];
    for (let i = 0; i < FAN_STEPS; i++) {
      const step = `Fan step ${i + 1}`;
      const temp = piNumber(`pi-fan-${i}-temp`, `${step}'s "on at"`, limits.fan_temp, 1000, " °C");
      const off = piNumber(`pi-fan-${i}-off`, `${step}'s "off below"`, limits.fan_temp, 1000, " °C");
      const speed = piNumber(`pi-fan-${i}-speed`, `${step}'s speed`, limits.fan_speed);
      const error = temp.error || off.error || speed.error;
      if (error) return { error };
      if (temp.value === undefined || off.value === undefined || speed.value === undefined) {
        return { error: `${step}: fill in every box, or turn the custom fan curve off.` };
      }
      const hyst = temp.value - off.value;
      if (hyst < limits.fan_hyst.min || hyst > limits.fan_hyst.max) {
        return {
          error: `${step}: "off below" must be ${limits.fan_hyst.min / 1000} to ${limits.fan_hyst.max / 1000} °C under "on at".`,
        };
      }
      const prev = s.fan[i - 1];
      if (prev && temp.value <= prev.temp) return { error: `${step} must turn on hotter than step ${i}.` };
      if (prev && speed.value < prev.speed) return { error: `${step} can't be slower than step ${i}.` };
      s.fan.push({ temp: temp.value, hyst, speed: speed.value });
    }
    if (s.fan[FAN_STEPS - 1].speed < limits.fan_top_speed) {
      return { error: `The last fan step must be at least ${limits.fan_top_speed}, so the fan runs when it's hot.` };
    }
  }
  return { settings: s };
}

function renderPiWarn() {
  const l = data.pi?.limits;
  const warn = [];
  if (l && Number($("pi-arm-freq").value) > l.arm_freq_warn) {
    warn.push(`Above ${l.arm_freq_warn} MHz many Pi 5s aren't stable. The trial boot will tell, and goes back if not.`);
  }
  if (Number($("pi-volts").value) > 0) warn.push("Extra voltage makes more heat: keep an eye on the temperature.");
  $("pi-warn").textContent = warn.join(" ");
  $("pi-warn").hidden = !warn.length;
}

function setPiNote(text, isError = false) {
  const note = $("pi-note");
  note.textContent = text;
  note.classList.toggle("error", isError);
}

const serverStopped = () => ["inactive", "failed"].includes(data.status?.unit?.active);

function renderPi() {
  const p = data.pi;
  const trial = p?.trial;
  const phase = trial?.phase;
  // A trial that is being checked, or waits for Keep, holds everything
  // else until it's settled.
  const trialBusy = !!p?.trial_boot && (phase === "checking" || phase === "passed");
  const can = !!p?.can_change && !!p?.supported && !piBusy;

  let locked = p?.locked || "";
  if (!p) locked = "Couldn't read the Pi's settings.";
  else if (!p.supported) locked = `These settings are for a Raspberry Pi 5, and this is ${p.model || "an unknown board"}.`;
  $("pi-locked").textContent = locked;
  $("pi-locked").hidden = !locked;

  const problems = $("pi-problems");
  problems.replaceChildren(
    ...(p?.problems ?? []).map((msg) => {
      const li = document.createElement("li");
      li.textContent = msg;
      return li;
    }),
  );
  problems.hidden = !p?.problems?.length;

  // The last trial.
  const banner = $("pi-trial");
  const info = PI_TRIAL[phase];
  banner.hidden = !info;
  if (info) {
    banner.dataset.phase = info.level;
    $("pi-trial-title").textContent = `${info.title} · ${fmtDate(trial.at)}`;
    $("pi-trial-text").textContent = `${trial.message} Tried: ${describePi(trial.settings)}.`;
  }
  const deciding = !!p?.trial_boot && phase === "passed";
  $("pi-keep").hidden = $("pi-back").hidden = !deciding;
  $("pi-keep").disabled = $("pi-back").disabled = !can;

  $("pi-saved").textContent = p ? describePi(p.saved) : "–";
  $("pi-pending").textContent = p ? describePi(p.pending) : "–";
  $("pi-pending-title").textContent = trialBusy ? "On trial now" : "Waiting for a trial boot";

  if (p) {
    const l = p.limits;
    $("pi-arm-freq").placeholder = l.arm_freq.default;
    $("pi-arm-freq").min = l.arm_freq.min;
    $("pi-arm-freq").max = l.arm_freq.max;
    $("pi-arm-freq-range").textContent = `${l.arm_freq.min}–${l.arm_freq.max}, default ${l.arm_freq.default}`;
    $("pi-volts").placeholder = l.over_voltage_delta.default / 1000;
    $("pi-volts").min = l.over_voltage_delta.min / 1000;
    $("pi-volts").max = l.over_voltage_delta.max / 1000;
    $("pi-volts-range").textContent = `${l.over_voltage_delta.min / 1000}–${l.over_voltage_delta.max / 1000}, default 0`;
    $("pi-temp-limit").placeholder = l.temp_limit.default;
    $("pi-temp-limit").min = l.temp_limit.min;
    $("pi-temp-limit").max = l.temp_limit.max;
    $("pi-temp-limit-range").textContent = `${l.temp_limit.min}–${l.temp_limit.max}, default ${l.temp_limit.default}`;
    if (!piDirty) fillPiForm(p.pending ?? p.saved, l);
  }
  const editable = can && !trialBusy;
  for (const id of ["pi-arm-freq", "pi-volts", "pi-temp-limit", "pi-fan-custom"]) $(id).disabled = !editable;
  const fanOn = $("pi-fan-custom").checked;
  for (let i = 0; i < FAN_STEPS; i++) {
    for (const part of ["temp", "off", "speed"]) fanCell(i, part).disabled = !editable || !fanOn;
  }
  $("pi-fan").classList.toggle("off", !fanOn);
  $("pi-save").disabled = !editable;
  $("pi-defaults").disabled = !editable;
  $("pi-discard").hidden = !p?.pending || trialBusy;
  $("pi-discard").disabled = !editable;
  renderPiWarn();

  // Rebooting: onto the waiting settings, or back after Go back.
  const goingBack = !!p?.trial_boot && phase === "discarded";
  $("pi-reboot").hidden = !goingBack;
  $("pi-reboot").disabled = !can || !serverStopped();
  $("pi-reboot-trial").hidden = goingBack;
  let why = "";
  if (!p?.pending) why = "Save settings first; then reboot to try them.";
  else if (trialBusy) why = "Keep or go back first.";
  else if (!p.watchdog)
    why = "The hardware watchdog isn't running, so a trial that hangs couldn't reset itself. See deploy/pi-firmware/README.md.";
  else if (!serverStopped()) why = "Stop the server first (Server section), so Paper saves the world.";
  if (goingBack) why = serverStopped() ? "" : "Stop the server first (Server section), then reboot to go back.";
  $("pi-reboot-trial").disabled = !can || !!why;
  $("pi-reboot-hint").textContent = can || !p ? why : "";

  // The governor.
  const select = $("pi-governor");
  const govs = p?.governors ?? [];
  if (select.options.length !== govs.length || [...select.options].some((o, i) => o.value !== govs[i])) {
    select.replaceChildren(...govs.map((g) => new Option(g, g)));
  }
  if (document.activeElement !== select && p?.governor) select.value = p.governor;
  select.disabled = !can || !govs.length;
  $("pi-governor-apply").disabled = !can || !govs.length;
}

async function piRequest(doing, path, body, done) {
  piBusy = true;
  renderPi();
  setPiNote(doing);
  let ok = false;
  try {
    await post(path, body);
    setPiNote(done);
    ok = true;
  } catch (err) {
    setPiNote(err.message, true);
  } finally {
    piBusy = false;
  }
  await refreshPi();
  return ok;
}

// Asks for the word to be typed before a reboot, and resolves with it, or
// "" if cancelled.
function confirmReboot(trial) {
  const dialog = $("pi-confirm");
  const word = $("pi-confirm-word");
  $("pi-confirm-title").textContent = trial ? "Reboot to try the new settings?" : "Reboot the Pi?";
  $("pi-confirm-message").textContent =
    (trial
      ? `The Pi reboots once with ${describePi(data.pi?.pending)}, then checks itself for about two minutes. If it hangs or a check fails, it reboots back to the saved settings.`
      : "The Pi reboots onto its saved settings.") +
    ` admin is gone for a minute or two. Type "${REBOOT_WORD}" to confirm.`;
  word.value = "";
  $("pi-confirm-ok").disabled = true;
  dialog.returnValue = "";
  dialog.showModal();
  word.focus();
  return new Promise((resolve) =>
    dialog.addEventListener(
      "close",
      () => resolve(dialog.returnValue === "ok" && word.value.trim() === REBOOT_WORD ? REBOOT_WORD : ""),
      { once: true },
    ),
  );
}
$("pi-confirm-word").addEventListener("input", (e) => {
  $("pi-confirm-ok").disabled = e.target.value.trim() !== REBOOT_WORD;
});

async function rebootPi(trial) {
  const confirm = await confirmReboot(trial);
  if (!confirm) return;
  await piRequest(
    "Asking the Pi to reboot…",
    "/api/pi/reboot",
    { confirm, trial },
    trial
      ? "Rebooting. admin is back in a minute or two, then the Pi checks itself for about two more."
      : "Rebooting. admin is back in a minute or two.",
  );
}

$("pi-form").addEventListener("input", () => {
  piDirty = true;
  renderPi();
});
$("pi-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const { settings, error } = readPiForm(data.pi.limits);
  if (error) return setPiNote(error, true);
  if (await piRequest("Saving…", "/api/pi/settings", settings, "Saved for a trial boot. Nothing changes until the Pi reboots.")) {
    piDirty = false;
    renderPi();
  }
});
$("pi-defaults").addEventListener("click", () => {
  fillPiForm(null, data.pi.limits);
  piDirty = true;
  renderPi();
});
$("pi-discard").addEventListener("click", async () => {
  if (await piRequest("Discarding…", "/api/pi/settings/discard", {}, "Discarded. The next reboot uses the saved settings.")) {
    piDirty = false;
    renderPi();
  }
});
$("pi-keep").addEventListener("click", async () => {
  const ok = await ask(
    "Keep these settings?",
    "They're copied into config.txt, after a backup, and every boot uses them from now on.",
    "Keep",
  );
  if (ok) await piRequest("Keeping…", "/api/pi/settings/keep", {}, "Kept. config.txt was backed up first.");
});
$("pi-back").addEventListener("click", async () => {
  const ok = await ask(
    "Go back to the saved settings?",
    "The trial settings stay until the Pi reboots; reboot afterwards to switch back.",
    "Go back",
  );
  if (ok) await piRequest("Going back…", "/api/pi/settings/discard", {}, "Reboot to switch back to the saved settings.");
});
$("pi-reboot-trial").addEventListener("click", () => rebootPi(true));
$("pi-reboot").addEventListener("click", () => rebootPi(false));
$("pi-governor-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const governor = $("pi-governor").value;
  piRequest(`Switching to ${governor}…`, "/api/pi/governor", { governor }, `The CPU governor is ${governor} now.`);
});

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
  refreshBackups();
  refreshPlayers();
  refreshHealth();
  refreshPi();
  refreshMe().then(refreshLogs);
  timers = [
    setInterval(refreshStatus, STATUS_MS),
    setInterval(refreshDeploy, DEPLOY_MS),
    setInterval(refreshPlugins, PLUGINS_MS),
    setInterval(refreshBackups, BACKUPS_MS),
    setInterval(refreshPlayers, PLAYERS_MS),
    setInterval(refreshHealth, HEALTH_MS),
    setInterval(refreshPi, PI_MS),
    setInterval(refreshLogs, LOG_MS),
    setInterval(tick, 1000),
  ];
  if (fastUntil > Date.now()) pollFast();
}
function stop() {
  timers.forEach(clearInterval);
  timers = [];
  clearTimeout(fastTimer);
  clearTimeout(backupTimer);
}

// Don't poll while the tab is in the background.
document.addEventListener("visibilitychange", () => (document.hidden ? stop() : start()));
start();

// admin.ryancaudill.dev: polls the admin API and renders the dashboard.
// Updates only text and data-state, so polling never moves keyboard focus;
// the list of changes is rebuilt only when the release changes. The Server
// and Console sections POST actions and commands; their output is only
// ever set as text. The Players section edits the whitelist and ops over
// RCON. The Backups section lists the world backups and keeps, deletes,
// downloads and starts them.

const STATUS_MS = 10_000; // server state and who's online
const DEPLOY_MS = 60_000; // the active release (changes a few times a day)
const PLUGINS_MS = 120_000; // installed plugins (change only on a restart)
const HEALTH_MS = 15_000; // the Pi's readings and Paper's TPS (cached 10s)

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
const OUTCOMES = { success: "Succeeded", failed: "Failed", rolled_back: "Rolled back" };
const REPO = "https://github.com/RyanCaudill04/minecraft-server";

const data = { status: null, deploy: null, plugins: null, me: null, backups: null, players: null, health: null };

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
const GB = 1024 ** 3;
function fmtBytes(n) {
  if (n >= GB) return `${(n / GB).toFixed(n >= 100 * GB ? 0 : 1)}${NBSP}GB`;
  return `${Math.round(n / 1024 ** 2)}${NBSP}MB`;
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
  renderBackups();
  renderPlayers();
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

// ---------- Players ----------

const PLAYERS_MS = 30_000; // the lists (Paper saves them on each change)

// A Minecraft name. admin checks it again before anything reaches RCON.
const PLAYER_NAME = /^[A-Za-z0-9_]{3,16}$/;

const PLAYER_LISTS = {
  whitelist: {
    add: (n) => ({ doing: `Adding ${n}…` }),
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

  const state = $("whitelist-state");
  state.hidden = d?.white_list == null;
  state.dataset.kind = d?.white_list ? "active" : "rolled_back";
  state.textContent = d?.white_list ? (d.enforce_whitelist ? "On, enforced" : "On") : "Off";
  state.title = d?.white_list
    ? d.enforce_whitelist
      ? "white-list and enforce-whitelist are on: only these players can join, and anyone removed is kicked."
      : "white-list is on: only these players can join."
    : "white-list is off in server.properties, so anyone can join. This list is kept for when it's on.";

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
}

function setPlayerEmpty(id, text) {
  $(id).textContent = text;
  $(id).hidden = !text;
}

// Rebuilds a list when it changes, and puts focus back on the same
// button afterwards (or on the card, if that player is gone).
const shownPlayers = {};
function renderPlayerList(list, players, can) {
  const key = JSON.stringify([players, can]);
  if (key === shownPlayers[list]) return;
  shownPlayers[list] = key;

  const ul = $(`${list}-list`);
  const active = document.activeElement;
  const refocus = ul.contains(active) ? active.dataset.name : null;

  ul.replaceChildren(
    ...players.map((p) => {
      const li = document.createElement("li");
      const who = document.createElement("span");
      who.className = "who";
      const name = document.createElement("span");
      name.className = "title";
      name.textContent = p.name;
      const id = document.createElement("span");
      id.className = "file";
      id.textContent = p.uuid;
      who.append(name, id);
      li.append(who);

      if (list === "ops") {
        const level = document.createElement("span");
        level.className = "kind";
        level.textContent = `Level ${p.level}`;
        level.title = "Operator permission level, 1 to 4.";
        li.append(level);
      }

      const b = button(list === "ops" ? "Deop" : "Remove", "remove", p.name, "btn-secondary");
      b.setAttribute("aria-label", list === "ops" ? `Deop ${p.name}` : `Remove ${p.name} from the whitelist`);
      b.disabled = !can;
      b.addEventListener("click", () => changePlayer(list, "remove", p.name));
      li.append(b);
      return li;
    }),
  );

  if (refocus) {
    const again = [...ul.querySelectorAll("[data-name]")].find((n) => n.dataset.name === refocus && !n.disabled);
    (again ?? ul.closest(".card")).focus();
  }
}

async function changePlayer(list, action, name) {
  if (playersBusy) return false;
  if (!PLAYER_NAME.test(name)) {
    setPlayersNote("A Minecraft name is 3 to 16 letters, digits or underscores.", true);
    return false;
  }
  const step = PLAYER_LISTS[list][action](name);
  if (step.ask && !(await ask(...step.ask))) return false;

  playersBusy = true;
  renderPlayers();
  setPlayersNote(step.doing);
  let ok = false;
  try {
    const { output } = await post(`/api/players/${list}/${action}`, { name });
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

function fmtBytes(n) {
  if (n == null) return "–";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n < 10 && i ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
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
  const key = JSON.stringify([list, can, Math.floor(Date.now() / 3_600_000)]);
  if (key === shownBackups) return;
  shownBackups = key;

  const active = document.activeElement;
  const refocus = $("backup-list").contains(active) ? [active.dataset.name, active.dataset.op] : null;

  $("backup-list").replaceChildren(
    ...list.map((b) => {
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
  refreshMe().then(refreshLogs);
  timers = [
    setInterval(refreshStatus, STATUS_MS),
    setInterval(refreshDeploy, DEPLOY_MS),
    setInterval(refreshPlugins, PLUGINS_MS),
    setInterval(refreshBackups, BACKUPS_MS),
    setInterval(refreshPlayers, PLAYERS_MS),
    setInterval(refreshHealth, HEALTH_MS),
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

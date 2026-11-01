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

// ---------- Releases and update policy ----------

const SETTINGS_MS = 15_000; // the policy and the newest release seen
const WATCH_MS = 2_000; // while mc-agent carries out a change
const WATCH_FOR_MS = 4 * 60_000; // a deploy waits up to a minute per health URL
const MODE_HELP = {
  auto: "mc-agent installs each new release.",
  notify: "mc-agent shows new releases here but doesn't install them.",
  pinned: "mc-agent stays on one release. Pick one under Deploys → Releases.",
};
const ACTIONS = {
  auto: "follow the latest release",
  notify: "only notify about new releases",
  pin: "apply",
  pause: "pause updates",
  resume: "resume updates",
};

data.settings = null;
data.releases = null;
let watching = null; // {since, label} while waiting for mc-agent

async function refreshSettings() {
  try {
    data.settings = await getJSON("/api/settings");
  } catch (err) {
    // admin restarts during a deploy, so this is expected while watching.
    console.warn(err);
    data.settings = null;
  }
  renderSettings();
}

async function refreshReleases() {
  try {
    data.releases = await getJSON("/api/releases");
  } catch (err) {
    console.warn(err);
    data.releases = null;
  }
  renderSettings(); // the notify row's Apply depends on the list too
}

// Sends a change. Same-origin only, with the header admin requires.
async function send(path, body) {
  let res;
  try {
    res = await fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Admin-Request": "1" },
      body: JSON.stringify(body),
    });
  } catch {
    return "Couldn't reach admin.";
  }
  if (res.ok) return "";
  const err = await res.json().catch(() => ({}));
  return err.error || `HTTP ${res.status}`;
}

function track() {
  return data.settings?.tracks?.[0];
}

function renderSettings() {
  const s = data.settings;
  const t = track();
  const can = !!s?.can_change && !watching;

  $("settings-locked").hidden = !s?.locked;
  $("settings-locked").textContent = s?.locked ?? "";
  $("track-active").textContent = t ? t.active || "None" : "–";
  $("track-available").textContent = t?.available || "–";
  $("track-checked").textContent = t?.checked_at ? `checked ${fmtAgo(t.checked_at)}` : "";

  for (const b of document.querySelectorAll(".segmented button")) {
    b.setAttribute("aria-checked", String(t?.mode === b.dataset.mode));
    b.disabled = !can;
  }
  $("mode-help").textContent = t ? MODE_HELP[t.mode] ?? "" : "Couldn't read the update policy.";

  // A newer release than the active one, held back by notify.
  const newer = t && t.mode === "notify" && t.available && t.available !== t.active ? t.available : "";
  $("notify-row").hidden = !newer;
  $("notify-text").textContent = newer ? `${newer} available` : "";
  $("notify-apply").disabled = !can || !isSelectable(newer);
  $("notify-notes").href = newer ? `${REPO}/releases/tag/${encodeURIComponent(newer)}` : "#";

  $("pause-toggle").checked = !!s?.paused;
  $("pause-toggle").disabled = !can;

  renderBanner();
  renderChange();
  renderReleases();
}

function renderBanner() {
  const s = data.settings;
  const t = track();
  let text = "";
  let action = "";
  if (s?.paused) {
    text = "All updates are paused. mc-agent won't deploy anything until they're resumed.";
    action = "resume";
  } else if (t?.mode === "pinned") {
    text = `Pinned to ${t.tag}, not following the latest release.`;
    action = "auto";
  }
  $("policy-banner").hidden = !text;
  $("policy-banner-text").textContent = text;
  const btn = $("banner-action");
  btn.hidden = !action || !s?.can_change;
  btn.disabled = !!watching;
  btn.dataset.action = action;
  btn.textContent = action === "resume" ? "Resume updates" : "Follow latest";
}

function describe(c) {
  return c.action === "pin" ? `apply ${c.tag}` : ACTIONS[c.action] ?? c.action;
}

function renderChange() {
  const s = data.settings;
  let text = "None yet.";
  let at = "";
  if (watching) {
    text = `Waiting for mc-agent to ${watching.label}…`;
    if (!s) text += " admin is restarting.";
  } else if (s?.pending) {
    text = `Waiting for mc-agent to ${describe(s.pending)}.`;
    at = `asked ${fmtAgo(s.pending.at)}`;
  } else if (s?.last) {
    text = s.last.error ? `Couldn't ${describe(s.last)}: ${s.last.error}` : `Done: ${describe(s.last)}.`;
    at = fmtAgo(s.last.done_at);
  }
  $("change-status").textContent = text;
  $("change-at").textContent = at;
}

function isSelectable(tag) {
  return !!data.releases?.releases?.find((r) => r.tag === tag)?.selectable;
}

let shownReleases;
function renderReleases() {
  const l = data.releases;
  const can = !!data.settings?.can_change && !watching;
  const note = !l ? "Couldn't read the releases." : l.github_error || (l.releases.length ? "" : "No releases yet.");
  $("release-note").textContent = note;
  $("release-note").hidden = !note;

  // Rebuilt only when something shown changes, so polling never moves focus.
  const key = JSON.stringify([l?.releases, can]);
  if (key === shownReleases) return;
  shownReleases = key;

  $("release-list").replaceChildren(
    ...(l?.releases ?? []).map((r) => {
      const li = document.createElement("li");
      const tag = document.createElement("code");
      tag.className = "tag";
      tag.textContent = r.tag;
      const when = document.createElement("span");
      when.className = "at";
      when.textContent = r.published_at
        ? new Date(r.published_at).toLocaleDateString([], { month: "short", day: "numeric" })
        : "installed";
      const badges = document.createElement("span");
      badges.className = "badges";
      for (const [on, label, kind] of [
        [r.active, "Active", "feature"],
        [r.pinned, "Pinned", "pinned"],
        [r.installed && !r.active, "Installed", "other"],
        [r.failed, "Failed", "failed"],
      ]) {
        if (!on) continue;
        const b = document.createElement("span");
        b.className = "kind";
        b.dataset.kind = kind;
        b.textContent = label;
        badges.append(b);
      }
      const notes = document.createElement("a");
      notes.href = r.url;
      notes.textContent = "Notes";
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "btn btn-small";
      btn.textContent = r.active ? "Pin here" : "Apply";
      btn.disabled = !can || !r.selectable || r.pinned;
      btn.addEventListener("click", () => applyRelease(r));
      li.append(tag, when, badges, notes, btn);
      if (r.reason) {
        const why = document.createElement("span");
        why.className = "reason";
        why.textContent = r.reason;
        btn.title = r.reason;
        li.append(why);
      }
      return li;
    }),
  );
}

// Asks before a change, naming it. Resolves true to go ahead.
function confirmChange(title, text, warn, ok) {
  const dlg = $("confirm");
  $("confirm-title").textContent = title;
  $("confirm-text").textContent = text;
  $("confirm-warn").textContent = warn;
  $("confirm-warn").hidden = !warn;
  $("confirm-ok").textContent = ok;
  dlg.returnValue = "";
  dlg.showModal();
  return new Promise((resolve) => dlg.addEventListener("close", () => resolve(dlg.returnValue === "ok"), { once: true }));
}

async function change(path, body, label) {
  const err = await send(path, body);
  if (err) {
    $("change-status").textContent = `Couldn't ${label}: ${err}`;
    $("change-at").textContent = "";
    return;
  }
  watching = { since: Date.now(), label };
  renderSettings();
  watch();
}

// Polls until mc-agent has carried out the change, or gives up.
async function watch() {
  await refreshSettings();
  const s = data.settings;
  const done = s && !s.pending && s.last && new Date(s.last.done_at) >= watching.since - 5000;
  if (done || Date.now() - watching.since > WATCH_FOR_MS) {
    watching = null;
    await refreshReleases();
    refreshDeploy();
    renderSettings();
    return;
  }
  setTimeout(watch, WATCH_MS);
}

async function applyRelease(r) {
  const t = track();
  const from = t?.active || "nothing";
  const warn = [
    r.failed ? `${r.tag} failed a deploy before.` : "",
    data.settings?.paused ? "Updates are paused; this applies it anyway." : "",
    r.active ? "" : "play, admin and the sites restart, and this page reconnects.",
  ].filter(Boolean).join(" ");
  const ok = await confirmChange(
    r.active ? `Pin ${r.tag}?` : `Apply ${r.tag}?`,
    r.active ? `Stay on ${r.tag} and stop following the latest release.` : `${from} → ${r.tag}. It stays active until you follow the latest again.`,
    warn,
    r.active ? "Pin" : "Apply",
  );
  if (ok) change(`/api/settings/${t.track}`, { mode: "pinned", tag: r.tag }, `apply ${r.tag}`);
}

async function setMode(mode) {
  const t = track();
  if (!t || mode === t.mode) return;
  if (mode === "pinned") {
    $("releases").scrollIntoView();
    return;
  }
  const text = mode === "auto"
    ? `mc-agent installs the newest release${t.available && t.available !== t.active ? `, ${t.available}, now` : ""} and each one after it.`
    : "mc-agent shows new releases here and leaves the active one alone.";
  if (await confirmChange(mode === "auto" ? "Follow the latest release?" : "Only notify?", text, "", "Change")) {
    change(`/api/settings/${t.track}`, { mode }, ACTIONS[mode]);
  }
}

async function setPaused(paused) {
  const ok = await confirmChange(
    paused ? "Pause all updates?" : "Resume updates?",
    paused ? "mc-agent won't deploy anything until updates are resumed." : "mc-agent follows the update policy again on its next run.",
    "",
    paused ? "Pause" : "Resume",
  );
  if (ok) change("/api/pause", { paused }, paused ? "pause updates" : "resume updates");
  else renderSettings(); // put the switch back
}

for (const b of document.querySelectorAll(".segmented button")) b.addEventListener("click", () => setMode(b.dataset.mode));
$("pause-toggle").addEventListener("change", (e) => setPaused(e.target.checked));
$("notify-apply").addEventListener("click", () => {
  const r = data.releases?.releases?.find((x) => x.tag === track()?.available);
  if (r) applyRelease(r);
});
$("banner-action").addEventListener("click", (e) =>
  e.target.dataset.action === "resume" ? setPaused(false) : setMode("auto"),
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
  refreshSettings();
  refreshReleases();
  timers = [
    setInterval(refreshStatus, STATUS_MS),
    setInterval(refreshDeploy, DEPLOY_MS),
    // watch() polls faster while a change is under way.
    setInterval(() => watching || refreshSettings(), SETTINGS_MS),
    setInterval(() => watching || refreshReleases(), 5 * 60_000),
    setInterval(tick, 1000),
  ];
}
function stop() {
  timers.forEach(clearInterval);
  timers = [];
}

// Don't poll while the tab is in the background.
document.addEventListener("visibilitychange", () => (document.hidden ? stop() : start()));
start();

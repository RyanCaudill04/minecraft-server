// play.ryancaudill.dev: polls the play API and renders the dashboard.

import { accentHue, capitalize, fmtAgo, fmtDistance, fmtHours, fmtNum, fmtPlaytime, fmtWhen, mcClock, phase, weather } from "./format.js";

const STATUS_MS = 10_000; // server state and who's online
const DATA_MS = 60_000; // world facts and player stats (change only on save)

// Paper sends this UUID for players who hide themselves from the list.
const ANONYMOUS_ID = "00000000-0000-0000-0000-000000000000";

const STATES = {
  active: {
    title: "Online",
    message: () => "The server is up. Jump in!",
  },
  sleeping: {
    title: "Sleeping",
    message: (world) => (world?.lastSaved ? `Asleep since ${fmtWhen(world.lastSaved)}.` : ""),
  },
  starting: {
    title: "Starting up",
    message: () => "Almost ready. Try joining in a moment.",
  },
  offline: {
    title: "Offline",
    message: () => "The server is down right now. Check back later.",
  },
  unknown: {
    title: "Can't reach status",
    message: () => "Retrying…",
  },
};

const BOARDS = [
  { key: "playTicks", label: "Playtime", fmt: fmtPlaytime },
  { key: "mobKills", label: "Mob kills", fmt: fmtNum },
  { key: "blocksMined", label: "Blocks mined", fmt: fmtNum },
  { key: "distanceCm", label: "Distance", fmt: fmtDistance },
  { key: "advancements", label: "Advancements", fmt: fmtNum },
  { key: "fishCaught", label: "Fish caught", fmt: fmtNum },
  { key: "deaths", label: "Deaths", fmt: fmtNum },
];

const SHEET_STATS = [
  ...BOARDS,
  { key: "jumps", label: "Jumps", fmt: fmtNum },
  { key: "animalsBred", label: "Animals bred", fmt: fmtNum },
  { key: "playerKills", label: "Player kills", fmt: fmtNum },
];

const data = {
  status: null,
  world: null,
  players: [],
  board: BOARDS[0].key,
  rendered: new Set(), // sections that already played their entrance
  lastChecked: null, // from the last status that loaded, kept through outages
  boardShown: null, // board whose bars last grew in
  hue: 212, // last accent hue set, matching --accent-h's initial value
  sheetId: null, // player in the open sheet
};

const $ = (id) => document.getElementById(id);

// ---------- Fetching ----------

async function getJSON(path) {
  const res = await fetch(path, { cache: "no-store" });
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
  return res.json();
}

async function refreshStatus() {
  const before = data.status?.state;
  try {
    data.status = await getJSON("/api/status");
    data.lastChecked = data.status.checked;
  } catch (err) {
    console.warn(err);
    data.status = { state: "unknown", online: 0, max: 0, players: [] };
  }
  renderStatus();
  renderPlayers();
  // Joining or leaving changes stats, so reload them when the state flips.
  if (before && before !== data.status.state) refreshData();
}

async function refreshData() {
  const [world, players] = await Promise.allSettled([getJSON("/api/world"), getJSON("/api/players")]);
  if (world.status === "fulfilled") data.world = world.value;
  if (players.status === "fulfilled") data.players = players.value;
  renderStatus();
  renderWorld();
  renderPlayers();
  renderBoard();
}

// ---------- Rendering ----------

function renderStatus() {
  const s = data.status;
  if (!s) return;
  const info = STATES[s.state] ?? STATES.unknown;

  for (const el of document.querySelectorAll("[data-state]")) el.dataset.state = s.state;
  $("pill-text").textContent = info.title;
  $("state-title").textContent = info.title;
  $("state-message").textContent = info.message(data.world);

  countTo($("online"), s.online);
  $("max").textContent = s.max ? `/ ${s.max}` : "";

  const online = visibleOnline();
  $("online-avatars").replaceChildren(...online.slice(0, 8).map((p) => head(p, 28)));

  let sub;
  if (s.state === "active") {
    sub = onlineNames(s.online, online);
  } else if (data.players[0]?.lastSeen) {
    const last = data.players[0];
    sub = `Last played ${fmtAgo(last.lastSeen)} by ${last.name}`;
  } else {
    sub = " ";
  }
  $("online-sub").textContent = sub;
}

// The names in the sample, plus a count of the rest. The count comes from
// `online`: anonymous players, hide-online-players and the sample cap all
// leave people out of the sample.
function onlineNames(count, visible) {
  if (!count) return "Nobody's on yet. Be the first!";
  const hidden = count - visible.length;
  if (!visible.length) return `${count} ${count === 1 ? "player" : "players"} online`;
  const names = visible.map((p) => p.name).join(", ");
  return hidden > 0 ? `${names} and ${hidden} ${hidden === 1 ? "other" : "others"}` : names;
}

function renderWorld() {
  const w = data.world;
  if (!w) return;

  // The browser animates --accent-h as a plain number, so 350 to 10 would
  // sweep the whole wheel. Pick the equivalent hue nearest the last one.
  const hue = accentHue(w.timeOfDay);
  data.hue = hue + 360 * Math.round((data.hue - hue) / 360);
  document.documentElement.style.setProperty("--accent-h", data.hue);

  $("clock").textContent = mcClock(w.timeOfDay);
  $("clock-sub").textContent = `Day ${fmtNum(w.day)} · ${phase(w.timeOfDay)} · ${weather(w)}`;
  placeSky(w.timeOfDay);

  const facts = [
    ["Day", fmtNum(w.day), phase(w.timeOfDay)],
    ["Time", mcClock(w.timeOfDay), "As of the last save"],
    ["Weather", weather(w), w.raining ? "Bring a roof" : "Clear skies"],
    ["World age", fmtHours(w.ageTicks), "Real time the world has run"],
    ["Difficulty", capitalize(w.difficulty) + (w.hardcore ? " · Hardcore" : ""), w.hardcore ? "One life" : "Respawns on"],
    ["Spawn", w.spawn ? w.spawn.join(", ") : "Unknown", "X, Y, Z"],
    ["Version", w.version || "Unknown", "Java Edition"],
    ["Last saved", fmtAgo(w.lastSaved), w.lastSaved ? fmtWhen(w.lastSaved) : "Unknown"],
    ["Players", fmtNum(data.players.length), "Have joined so far"],
  ];
  const appear = once("world");
  $("facts").replaceChildren(
    ...facts.map(([label, value, hint], i) => {
      const card = el("article", "card fact" + (appear ? " appear" : ""));
      card.style.animationDelay = `${i * 30}ms`;
      card.append(el("span", "label", label), el("span", "value", value), el("span", "hint", hint));
      return card;
    }),
  );
}

function renderPlayers() {
  const onlineIDs = new Set(visibleOnline().map((p) => p.id));
  // Online players first, then most recently seen (the API's order).
  const players = [...data.players].sort((a, b) => onlineIDs.has(b.id) - onlineIDs.has(a.id));

  const appear = players.length > 0 && once("players");
  $("player-grid").replaceChildren(
    ...players.map((p, i) => {
      const card = el("button", "player-card" + (appear ? " appear" : ""));
      card.type = "button";
      card.style.animationDelay = `${i * 30}ms`;
      card.addEventListener("click", () => openSheet(p.id, card));

      const who = el("span", "who");
      const meta = el("span", "meta");
      if (onlineIDs.has(p.id)) {
        meta.append(el("span", "online-badge", "● Online"));
      } else {
        meta.textContent = p.lastSeen ? `Seen ${fmtAgo(p.lastSeen)}` : "Never seen";
      }
      meta.append(` · ${fmtPlaytime(p.playTicks)}`);
      who.append(el("span", "name", p.name), meta);
      card.append(head(p, 44), who);
      return card;
    }),
  );

  // Both polls end here, so the open sheet follows joins, leaves and saves.
  if (data.sheetId && !$("sheet").hidden) fillSheet(data.sheetId);
}

function renderBoard() {
  const tabs = $("board-tabs");
  if (!tabs.childElementCount) {
    for (const b of BOARDS) {
      const tab = el("button", "", b.label);
      tab.type = "button";
      tab.setAttribute("role", "tab");
      tab.dataset.key = b.key;
      tab.addEventListener("click", () => {
        data.board = b.key;
        renderBoard();
      });
      tabs.append(tab);
    }
  }
  for (const tab of tabs.children) tab.setAttribute("aria-selected", tab.dataset.key === data.board);

  const board = BOARDS.find((b) => b.key === data.board);
  const ranked = rankedBy(board.key);
  const top = Math.max(1, ranked[0]?.[board.key] ?? 1);
  // Bars grow in when a board first shows, not on every minute's refresh.
  const grow = ranked.length > 0 && data.boardShown !== board.key;
  if (grow) data.boardShown = board.key;

  const rows = ranked.map((p, i) => {
    const li = el("li");
    li.addEventListener("click", () => openSheet(p.id, li));
    const wrap = el("div", "bar-wrap");
    const bar = el("div", "bar");
    const fill = el("span");
    bar.append(fill);
    wrap.append(el("span", "name", p.name), bar);
    li.append(el("span", "rank", String(i + 1)), head(p, 32), wrap, el("span", "score", board.fmt(p[board.key])));
    const width = `${(p[board.key] / top) * 100}%`;
    // Set the width after insertion so the bar animates from zero, or
    // before it so the bar just appears at its size.
    if (grow) requestAnimationFrame(() => requestAnimationFrame(() => (fill.style.width = width)));
    else fill.style.width = width;
    return li;
  });
  $("board").replaceChildren(...rows);
}

// ---------- Player sheet ----------

let sheetOpener = null;

function openSheet(id, opener) {
  if (!fillSheet(id)) return;
  sheetOpener = opener;

  $("backdrop").hidden = false;
  $("sheet").hidden = false;
  $("sheet-close").focus();
}

// Fills the sheet with a player's stats. Returns false if they're unknown.
function fillSheet(id) {
  const p = data.players.find((x) => x.id === id);
  if (!p) return false;
  data.sheetId = id;

  $("sheet-head").src = headURL(p.id, 144);
  $("sheet-name").textContent = p.name;
  const online = visibleOnline().some((x) => x.id === id);
  $("sheet-seen").textContent = online ? "Online now" : p.lastSeen ? `Last seen ${fmtAgo(p.lastSeen)}` : "Never seen";

  $("sheet-stats").replaceChildren(
    ...SHEET_STATS.map((s) => {
      const div = el("div");
      const dd = el("dd", "", s.fmt(p[s.key]));
      const rank = rankedBy(s.key).findIndex((x) => x.id === id) + 1;
      if (rank === 1 && p[s.key] > 0 && s.key !== "deaths") dd.append(el("span", "rank-note", "#1 on the server"));
      div.append(el("dt", "", s.label), dd);
      return div;
    }),
  );
  return true;
}

function closeSheet() {
  if ($("sheet").hidden) return;
  $("backdrop").hidden = true;
  $("sheet").hidden = true;
  sheetOpener?.focus();
}

$("sheet-close").addEventListener("click", closeSheet);
$("backdrop").addEventListener("click", closeSheet);
document.addEventListener("keydown", (e) => e.key === "Escape" && closeSheet());

// ---------- Day/night ----------

// Moves the sun (day) or moon (night) along the arc in the time card.
function placeSky(tod) {
  const f = (tod % 12000) / 12000;
  const body = $("sky-body");
  body.setAttribute("cx", (50 - 42 * Math.cos(Math.PI * f)).toFixed(1));
  body.setAttribute("cy", (30 - 26 * Math.sin(Math.PI * f)).toFixed(1));
  body.setAttribute("r", tod < 12000 ? 4 : 3);
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

// ---------- Helpers ----------

function el(tag, className = "", text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function headURL(id, size) {
  return `https://mc-heads.net/avatar/${encodeURIComponent(id)}/${size}`;
}

// A player's skin head, or a blank tile if it's hidden or fails to load.
function head(player, size) {
  if (!player.id || player.id === ANONYMOUS_ID) return el("span", "head-fallback");
  const img = el("img");
  img.src = headURL(player.id, size * 2);
  img.width = img.height = size;
  img.alt = "";
  img.loading = "lazy";
  img.onerror = () => img.replaceWith(el("span", "head-fallback"));
  return img;
}

// Online players from the status sample, minus anonymous entries.
function visibleOnline() {
  if (data.status?.state !== "active") return [];
  return data.status.players.filter((p) => p.id !== ANONYMOUS_ID);
}

// Players sorted by a stat, highest first. The board and the sheet's #1
// notes both use it, so they always agree.
function rankedBy(key) {
  return [...data.players].sort((a, b) => b[key] - a[key]);
}

// Returns true the first time it's called for a key, so entrance animations
// play once and not on every refresh.
function once(key) {
  if (data.rendered.has(key)) return false;
  data.rendered.add(key);
  return true;
}

// Animates a number from its current value to `to`.
function countTo(node, to) {
  const from = Number(node.dataset.value ?? 0);
  node.dataset.value = to;
  if (from === to || matchMedia("(prefers-reduced-motion: reduce)").matches) {
    node.textContent = fmtNum(to);
    return;
  }
  const start = performance.now();
  const step = (now) => {
    const f = Math.min(1, (now - start) / 600);
    const eased = 1 - (1 - f) ** 3;
    node.textContent = fmtNum(Math.round(from + (to - from) * eased));
    if (f < 1) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
}

// ---------- Map preview ----------

// The preview is squaremap's own page in a frame. Before loading it, ask
// whether the map exists, so an unrendered map shows the placeholder instead
// of an error page. The frame loads once; the map refreshes its own tiles.
async function loadMapPreview() {
  const card = $("map-card");
  try {
    const res = await fetch("/map/", { method: "HEAD", cache: "no-store" });
    if (!res.ok) return false;
  } catch {
    return false;
  }
  const frame = $("map-frame");
  frame.addEventListener("load", () => (card.dataset.ready = "true"), { once: true });
  frame.src = "/map/";
  return true;
}

// Try again every minute until the map shows up.
async function mapPreviewLoop() {
  if (!(await loadMapPreview())) setTimeout(mapPreviewLoop, 60_000);
}
mapPreviewLoop();

// ---------- Startup ----------

// Each click restarts the reset, so the label always shows for 2 s.
let copyReset;
$("copy").addEventListener("click", async () => {
  const button = $("copy");
  try {
    await navigator.clipboard.writeText($("address").textContent);
    button.textContent = "Copied!";
  } catch {
    button.textContent = "Copy failed";
  }
  clearTimeout(copyReset);
  copyReset = setTimeout(() => (button.textContent = "Copy address"), 2000);
});

// Uses the last status that loaded, so during an outage the line keeps
// ageing and turns amber instead of saying "just now" forever.
function tick() {
  if (!data.lastChecked) return;
  const updated = $("updated");
  updated.textContent = `Updated ${fmtAgo(data.lastChecked)}`;
  updated.classList.toggle("stale", Date.now() - new Date(data.lastChecked) > 30_000);
}

let timers = [];
function start() {
  if (timers.length) return;
  refreshStatus();
  refreshData();
  timers = [setInterval(refreshStatus, STATUS_MS), setInterval(refreshData, DATA_MS), setInterval(tick, 1000)];
}
function stop() {
  timers.forEach(clearInterval);
  timers = [];
}

// Don't poll while the tab is in the background.
document.addEventListener("visibilitychange", () => (document.hidden ? stop() : start()));
start();

// play.ryancaudill.dev: polls the play API and renders the dashboard.

import { accentHue, capitalize, fmtAgo, fmtDate, fmtDistance, fmtHours, fmtNum, fmtPlaytime, fmtWhen, mcClock, phase, weather } from "./format.js";

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

// Badges for the milestones the server tracks (world.Firsts in Go), in the
// order the sheet shows them.
const FIRSTS = {
  "minecraft:story/enter_the_nether": { icon: "🔥", label: "First into the Nether" },
  "minecraft:story/mine_diamond": { icon: "💎", label: "First diamonds" },
  "minecraft:story/enter_the_end": { icon: "🌌", label: "First into the End" },
  "minecraft:end/kill_dragon": { icon: "🐉", label: "Slew the dragon first" },
  "minecraft:nether/summon_wither": { icon: "💀", label: "First to summon the Wither" },
  "minecraft:end/elytra": { icon: "🪽", label: "First wings" },
};

const data = {
  status: null,
  world: null,
  players: [],
  activity: null, // recent events, newest first; null until one load works
  activityFailed: false,
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
  const [world, players, activity] = await Promise.allSettled([
    getJSON("/api/world"),
    getJSON("/api/players"),
    getJSON("/api/activity"),
  ]);
  if (world.status === "fulfilled") data.world = world.value;
  if (players.status === "fulfilled") data.players = players.value;
  if (activity.status === "fulfilled") data.activity = activity.value;
  data.activityFailed = activity.status === "rejected";
  renderStatus();
  renderWorld();
  renderPlayers();
  renderBoard();
  renderActivity();
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

// Player cards by id. They're updated in place instead of rebuilt on every
// status poll, so a focused card keeps focus and hover doesn't flicker.
const playerCards = new Map();

function renderPlayers() {
  const onlineIDs = new Set(visibleOnline().map((p) => p.id));
  // Online players first, then most recently seen (the API's order).
  const players = [...data.players].sort((a, b) => onlineIDs.has(b.id) - onlineIDs.has(a.id));

  const ids = new Set(players.map((p) => p.id));
  for (const id of playerCards.keys()) if (!ids.has(id)) playerCards.delete(id);

  const appear = players.length > 0 && once("players");
  const cards = players.map((p, i) => {
    let card = playerCards.get(p.id);
    if (!card) {
      card = el("button", "player-card" + (appear ? " appear" : ""));
      card.type = "button";
      card.dataset.id = p.id;
      card.style.animationDelay = `${i * 30}ms`;
      card.addEventListener("click", () => openSheet(p.id, "player-grid"));
      card.append(head(p, 44), el("span", "who"));
      playerCards.set(p.id, card);
    }

    const meta = el("span", "meta");
    if (onlineIDs.has(p.id)) {
      meta.append(el("span", "online-badge", "● Online"));
    } else {
      meta.textContent = p.lastSeen ? `Seen ${fmtAgo(p.lastSeen)}` : "Never seen";
    }
    meta.append(` · ${fmtPlaytime(p.playTicks)}`);
    card.querySelector(".who").replaceChildren(el("span", "name", p.name), meta);
    return card;
  });

  // Moving a node drops its focus, so only reorder when the order changed,
  // and then put focus back on the card that had it.
  const grid = $("player-grid");
  if (cards.length !== grid.children.length || cards.some((c, i) => grid.children[i] !== c)) {
    const focused = document.activeElement;
    grid.replaceChildren(...cards);
    if (cards.includes(focused)) focused.focus({ preventScroll: true });
  }

  // Both polls end here, so the open sheet follows joins, leaves and saves.
  if (data.sheetId && $("sheet").open) fillSheet(data.sheetId);
}

let boardKey = ""; // what the board last rendered, to skip unchanged refreshes

function renderBoard() {
  // A segmented control of toggle buttons rather than ARIA tabs: it's
  // simpler and it's what this is, one button per board.
  const tabs = $("board-tabs");
  if (!tabs.childElementCount) {
    for (const b of BOARDS) {
      const tab = el("button", "", b.label);
      tab.type = "button";
      tab.dataset.key = b.key;
      tab.addEventListener("click", () => {
        data.board = b.key;
        renderBoard();
      });
      tabs.append(tab);
    }
  }
  for (const tab of tabs.children) tab.setAttribute("aria-pressed", tab.dataset.key === data.board);

  const board = BOARDS.find((b) => b.key === data.board);
  const ranked = rankedBy(board.key);
  const top = Math.max(1, ranked[0]?.[board.key] ?? 1);
  // Bars grow in when a board first shows, not on every minute's refresh.
  const grow = ranked.length > 0 && data.boardShown !== board.key;
  if (grow) data.boardShown = board.key;

  // Rebuilding replays the bars and drops focus from a row, so only do it
  // when the board or its numbers changed.
  const key = JSON.stringify([board.key, ranked.map((p) => [p.id, p.name, p[board.key]])]);
  if (key === boardKey) return;
  boardKey = key;

  const list = $("board");
  list.setAttribute("aria-label", `${board.label} leaderboard`);
  const focusedID = list.contains(document.activeElement) ? document.activeElement.dataset.id : null;

  const rows = ranked.map((p, i) => {
    // The row is a real button so it can be reached and opened from the
    // keyboard, like the player cards.
    const li = el("li");
    const row = el("button");
    row.type = "button";
    row.dataset.id = p.id;
    row.addEventListener("click", () => openSheet(p.id, "board"));
    const wrap = el("span", "bar-wrap");
    const bar = el("span", "bar");
    const fill = el("span");
    bar.append(fill);
    wrap.append(el("span", "name", p.name), bar);
    row.append(el("span", "rank", String(i + 1)), head(p, 32), wrap, el("span", "score", board.fmt(p[board.key])));
    li.append(row);
    const width = `${(p[board.key] / top) * 100}%`;
    // Set the width after insertion so the bar animates from zero, or
    // before it so the bar just appears at its size.
    if (grow) requestAnimationFrame(() => requestAnimationFrame(() => (fill.style.width = width)));
    else fill.style.width = width;
    return li;
  });
  list.replaceChildren(...rows);
  if (focusedID) focusPlayer("board", focusedID, { preventScroll: true });
}

// The feed is rebuilt on every data refresh, which also keeps its relative
// times current. Nothing in it takes focus, so rebuilding loses nothing.
function renderActivity() {
  const events = data.activity;
  const feed = $("feed");
  const empty = $("feed-empty");
  if (!events) {
    // Never loaded: keep "Loading…" until a load fails.
    if (data.activityFailed) empty.textContent = "Activity isn't available right now. Retrying…";
    return;
  }

  feed.hidden = events.length === 0;
  empty.hidden = events.length > 0;
  empty.textContent = "Nothing yet. Advancements and new players show up here after the server saves.";

  const appear = events.length > 0 && once("activity");
  feed.replaceChildren(
    ...events.map((e, i) => {
      const li = el("li", "feed-item" + (appear ? " appear" : ""));
      li.style.animationDelay = `${Math.min(i, 10) * 30}ms`;
      const line = el("p", "feed-line");
      line.append(el("strong", "", e.name));
      if (e.kind === "joined") {
        line.append(" joined for the first time");
      } else {
        line.append(" earned ", el("strong", "", e.title || "an advancement"));
      }
      // The relative time, with the full time beside it so it's there on
      // touch screens and for screen readers too, not only on hover.
      const when = el("time", "feed-when");
      when.dateTime = e.at;
      when.title = new Date(e.at).toLocaleString(undefined, { dateStyle: "full", timeStyle: "long" });
      when.append(el("span", "feed-ago", fmtAgo(e.at)), ` · ${fmtWhen(e.at)}`);
      const text = el("div", "feed-text");
      text.append(line, when);
      li.append(head({ id: e.playerId }, 32), text);
      return li;
    }),
  );
}

// ---------- Player sheet ----------

// Where the sheet was opened from: the player's id and the list it was in.
// Not the element, which may be re-rendered while the sheet is open.
let sheetOpener = null;

function openSheet(id, from) {
  if (!fillSheet(id)) return;
  sheetOpener = { id, from };

  // showModal traps focus, makes the page inert and closes on Esc.
  $("sheet").showModal();
}

// Fills the sheet with a player's stats. Returns false if they're unknown.
function fillSheet(id) {
  const p = data.players.find((x) => x.id === id);
  if (!p) return false;
  data.sheetId = id;

  $("sheet-skin").src = bodyURL(p.id, 120);
  $("sheet-name").textContent = p.name;
  const online = visibleOnline().some((x) => x.id === id);
  $("sheet-seen").textContent = online ? "Online now" : p.lastSeen ? `Last seen ${fmtAgo(p.lastSeen)}` : "Never seen";

  const badges = (p.firsts ?? []).filter((f) => FIRSTS[f.advancement]);
  $("sheet-firsts").hidden = badges.length === 0;
  $("sheet-badges").replaceChildren(
    ...badges.map((f) => {
      const { icon, label } = FIRSTS[f.advancement];
      const li = el("li", "badge");
      const text = el("span", "badge-text");
      text.append(el("span", "badge-label", label), el("span", "badge-date", fmtDate(f.at)));
      li.append(el("span", "badge-icon", icon), text);
      li.firstChild.setAttribute("aria-hidden", "true");
      return li;
    }),
  );

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

// Focuses the player's card or row in the list with the given id.
function focusPlayer(listID, id, options) {
  $(listID).querySelector(`[data-id="${CSS.escape(id)}"]`)?.focus(options);
}

const sheet = $("sheet");
sheet.addEventListener("close", () => {
  if (sheetOpener) focusPlayer(sheetOpener.from, sheetOpener.id);
  sheetOpener = null;
  data.sheetId = null;
  pressedOutside = false;
});
$("sheet-close").addEventListener("click", () => sheet.close());
// The dialog fills the viewport height on desktop, so a click on its empty
// part targets the dialog itself. Decide by where the press started instead.
let pressedOutside = false;
sheet.addEventListener("pointerdown", (e) => {
  const r = sheet.getBoundingClientRect();
  pressedOutside = e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom;
});
sheet.addEventListener("click", () => pressedOutside && sheet.close());

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

// Highlights the last section or subsection whose top has scrolled above a
// line 30% down the window, or the final one once the page is scrolled to
// the bottom. A group's toggle is highlighted while one of its sections is.
const navLinks = [...document.querySelectorAll(".nav a")];
const navToggles = [...document.querySelectorAll(".nav-toggle")];
const sections = [...document.querySelectorAll(".section, .subsection")];

function updateNav() {
  const line = innerHeight * 0.3;
  const atBottom = innerHeight + scrollY >= document.documentElement.scrollHeight - 2;
  let current = sections[0];
  for (const s of sections) if (s.getBoundingClientRect().top <= line) current = s;
  if (atBottom && scrollY > 0) current = sections.at(-1);
  if (scrollY < 8) current = sections[0];
  for (const a of navLinks) a.classList.toggle("active", a.hash === `#${current.id}`);
  for (const t of navToggles) t.classList.toggle("active", $(t.dataset.section).contains(current));
}
addEventListener("scroll", updateNav, { passive: true });
addEventListener("resize", updateNav);
updateNav();

// A toggle shows or hides its group's links; the CSS animates the group from
// aria-expanded, and a closed group is inert so its links can't be focused.
// In the phone top bar the group grows past the right edge, so once it has
// grown the bar scrolls to show it (in the sidebar it's already in view).
for (const t of navToggles) {
  const group = $(t.getAttribute("aria-controls"));
  const reveal = () => {
    if (t.getAttribute("aria-expanded") === "true") {
      group.querySelector("a:last-child").scrollIntoView({ block: "nearest", inline: "nearest", behavior: "smooth" });
    }
  };
  group.addEventListener("transitionend", (e) => {
    if (e.target === group && e.propertyName === "grid-template-columns") reveal();
  });
  t.addEventListener("click", () => {
    const open = t.getAttribute("aria-expanded") !== "true";
    t.setAttribute("aria-expanded", String(open));
    group.inert = !open;
    // With reduced motion there's no transition to wait for.
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) reveal();
  });
}

// ---------- Helpers ----------

function el(tag, className = "", text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

// A front view of the player's whole skin, size pixels wide and 2.4 times
// as tall.
function bodyURL(id, size) {
  return `https://mc-heads.net/body/${encodeURIComponent(id)}/${size}`;
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

// The preview ignores the pointer, so squaremap's own controls would show as
// buttons that do nothing. These URL options turn them off: the zoom and
// layer buttons, the sidebar of worlds and players, the coordinates box and
// the copy-link button.
const mapPreviewURL = (world) =>
  `/map/?world=${encodeURIComponent(world)}` +
  "&show_controls=false&show_sidebar=false&show_coordinates=false&show_link_button=false";

// The worlds squaremap has a map for, from its tiles/settings.json, or an
// empty set before it has written one.
async function renderedWorlds() {
  try {
    const res = await fetch("/map/tiles/settings.json", { cache: "no-store" });
    if (!res.ok) return new Set();
    const settings = await res.json();
    return new Set((settings.worlds ?? []).map((w) => w.name));
  } catch {
    return new Set();
  }
}

// Each card previews one world in a frame of squaremap's own page. A world
// squaremap doesn't list keeps its placeholder: squaremap would show its
// first world instead of an unknown one. A frame loads once; the map
// refreshes its own tiles. Returns whether every card is loaded.
async function loadMapPreviews() {
  const waiting = [...document.querySelectorAll(".map-card:not([data-loading])")];
  if (!waiting.length) return true;
  const worlds = await renderedWorlds();
  for (const card of waiting) {
    if (!worlds.has(card.dataset.world)) continue;
    card.dataset.loading = "";
    const frame = card.querySelector("iframe");
    frame.addEventListener("load", () => (card.dataset.ready = "true"), { once: true });
    frame.src = mapPreviewURL(card.dataset.world);
  }
  return !document.querySelector(".map-card:not([data-loading])");
}

// Try again every minute until every map shows up.
async function mapPreviewLoop() {
  if (!(await loadMapPreviews())) setTimeout(mapPreviewLoop, 60_000);
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

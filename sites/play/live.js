// play.ryancaudill.dev: the live clock and weather, and the activity feed.
// Loads after app.js and uses its state (data) and helpers.

const TIME_MS = 10_000; // in-game clock and weather
const FEED_MS = 10_000; // activity feed
const MAX_RUN_ON_S = 120; // stop running the clock on if polls stop answering

const live = {
  sky: null, // the last /api/time answer
  got: 0, // performance.now() when it arrived
  events: null,
  feedHadPlayers: false,
};

// ---------- Clock and weather ----------

async function refreshTime() {
  try {
    live.sky = await getJSON("/api/time");
    live.got = performance.now();
  } catch (err) {
    console.warn(err);
    return;
  }
  renderWorld();
}

// The world facts with the clock run on to now: the server answers with
// the time as of its poll and how fast it moves, and the page counts on
// between polls.
function liveWorld(w) {
  const s = live.sky;
  if (!w || !s) return w;
  const elapsed = Math.min(MAX_RUN_ON_S, (performance.now() - live.got) / 1000);
  const ticks = (s.day - 1) * 24000 + s.timeOfDay + Math.floor(s.ticksPerSecond * elapsed);
  return {
    ...w,
    day: Math.floor(ticks / 24000) + 1,
    timeOfDay: ticks % 24000,
    raining: s.raining,
    thundering: s.thundering,
    live: s.live,
  };
}

// Runs the clock on each second while the server is up. The facts are a
// handful of cards, so redrawing them is cheap.
function tickClock() {
  if (live.sky?.ticksPerSecond && data.world) renderWorld();
  // The feed's heads need the players list, which may arrive after it.
  if (live.events && !live.feedHadPlayers && data.players.length) renderFeed();
}

// ---------- Activity feed ----------

async function refreshFeed() {
  try {
    live.events = (await getJSON("/api/activity")).events;
  } catch (err) {
    console.warn(err);
    if (live.events) return; // keep showing what we had
    live.events = [];
  }
  renderFeed();
}

function renderFeed() {
  const events = live.events;
  if (!events) return;
  $("feed-sub").textContent =
    data.status?.state === "active" ? "Joins, deaths and advancements, as they happen." : "From the last session.";

  if (!events.length) {
    $("feed").replaceChildren(el("li", "feed-empty", "Nothing yet. Joins, deaths and advancements show up here."));
    return;
  }

  // Heads come by UUID from the players list; a name alone could match
  // someone else's account, so unknown players get a blank tile.
  const ids = new Map(data.players.map((p) => [p.name, p.id]));
  live.feedHadPlayers = data.players.length > 0;
  $("feed").replaceChildren(
    ...events.map((e) => {
      const li = el("li", `feed-item feed-${e.kind}`);
      const text = el("p", "feed-text");
      text.append(el("span", "name", e.player), ` ${feedVerb(e)}`);
      if (e.kind === "advancement") text.append(" ", el("span", "feed-title", e.title));
      const when = el("time", "feed-when", fmtAgo(e.time));
      when.dateTime = e.time;
      when.title = fmtWhen(e.time);
      li.append(el("span", "feed-mark"), head({ id: ids.get(e.player) }, 28), text, when);
      return li;
    }),
  );
}

// The rest of the sentence after the player's name.
function feedVerb(e) {
  switch (e.kind) {
    case "join":
      return "joined the game";
    case "leave":
      return "left the game";
    case "death":
      // The game's own message, which starts with the name.
      return e.text.startsWith(e.player) ? e.text.slice(e.player.length).trimStart() : "died";
    case "advancement":
      return "made the advancement";
    default:
      return "";
  }
}

// ---------- Startup ----------

let liveTimers = [];
function startLive() {
  if (liveTimers.length) return;
  refreshTime();
  refreshFeed();
  liveTimers = [
    setInterval(refreshTime, TIME_MS),
    setInterval(refreshFeed, FEED_MS),
    setInterval(tickClock, 1000),
    setInterval(renderFeed, 30_000), // keeps "5m ago" current
  ];
}
function stopLive() {
  liveTimers.forEach(clearInterval);
  liveTimers = [];
}

document.addEventListener("visibilitychange", () => (document.hidden ? stopLive() : startLive()));
startLive();

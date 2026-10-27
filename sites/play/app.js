// Polls /api/status and renders the server state and player list.

const POLL_MS = 10_000;

// Banner text for each state reported by the Go server.
const STATES = {
  active: ["Server is active", "Jump in!"],
  sleeping: ["Server is sleeping", "Joining wakes it up. That takes about 30 seconds."],
  starting: ["Server is starting…", "Almost ready. Try joining in a moment."],
  offline: ["Server is offline", "Check back later."],
  unreachable: ["Can't reach status", "Retrying…"],
};

// Paper sends this UUID for players who hide themselves from the list.
const ANONYMOUS_ID = "00000000-0000-0000-0000-000000000000";

const $ = (id) => document.getElementById(id);

let lastChecked = null;
let timer = null;

async function refresh() {
  try {
    const res = await fetch("/api/status", { cache: "no-store" });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    render(await res.json());
    document.body.classList.remove("stale");
  } catch (err) {
    console.warn("status check failed:", err);
    setBanner("unreachable");
    document.body.classList.add("stale");
  }
}

function setBanner(state) {
  const [title, message] = STATES[state] ?? STATES.unreachable;
  $("banner").dataset.state = state === "unreachable" ? "unknown" : state;
  $("state-title").textContent = title;
  $("state-message").textContent = message;
}

function render(status) {
  setBanner(status.state);
  lastChecked = new Date(status.checked);
  tick();

  $("version-row").hidden = !status.version;
  $("version").textContent = status.version ?? "";

  const players = $("players");
  players.hidden = status.state !== "active";
  if (players.hidden) return;

  $("player-count").textContent = `${status.online} / ${status.max}`;
  $("player-empty").hidden = status.online > 0;

  const list = $("player-list");
  list.replaceChildren(...status.players.map(playerItem));

  // Servers only send a sample of names (Paper sends up to 12).
  const more = status.online - status.players.length;
  $("player-more").hidden = more <= 0;
  $("player-more").textContent = `…and ${more} more`;
}

function playerItem(player) {
  const li = document.createElement("li");
  if (player.id && player.id !== ANONYMOUS_ID) {
    const img = document.createElement("img");
    img.src = `https://mc-heads.net/avatar/${encodeURIComponent(player.id)}/24`;
    img.alt = "";
    img.loading = "lazy";
    img.onerror = () => img.remove();
    li.append(img);
  }
  const name = document.createElement("span");
  name.textContent = player.name;
  li.append(name);
  return li;
}

// Updates the "Updated Ns ago" footer.
function tick() {
  if (!lastChecked) return;
  const secs = Math.max(0, Math.round((Date.now() - lastChecked) / 1000));
  $("updated").textContent = secs < 2 ? "Updated just now" : `Updated ${secs}s ago`;
}

function start() {
  if (timer) return;
  refresh();
  timer = setInterval(refresh, POLL_MS);
}

function stop() {
  clearInterval(timer);
  timer = null;
}

// Don't poll while the tab is in the background.
document.addEventListener("visibilitychange", () => {
  document.hidden ? stop() : start();
});

$("copy").addEventListener("click", async () => {
  const button = $("copy");
  try {
    await navigator.clipboard.writeText($("address").textContent);
    button.textContent = "Copied!";
  } catch {
    button.textContent = "Copy failed";
  }
  setTimeout(() => (button.textContent = "Copy"), 2000);
});

setInterval(tick, 1000);
start();

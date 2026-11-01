// Pure helpers for play.ryancaudill.dev: formatting and the day/night
// cycle. No DOM access, so tests/play can run them under Node.

// Accent hue at points through the Minecraft day (0 = sunrise). Hues are
// interpolated between them, the short way round the color wheel.
const SKY_HUES = [
  [0, 30], // sunrise orange
  [1500, 205], // morning blue
  [10500, 212], // afternoon blue
  [12000, 18], // sunset
  [13000, 280], // dusk purple
  [14500, 238], // night indigo
  [22000, 238],
  [23300, 280], // pre-dawn
  [24000, 30],
];

export function accentHue(tod) {
  for (let i = 1; i < SKY_HUES.length; i++) {
    const [t1, h1] = SKY_HUES[i];
    if (tod > t1) continue;
    const [t0, h0] = SKY_HUES[i - 1];
    const f = (tod - t0) / (t1 - t0);
    const delta = ((h1 - h0 + 540) % 360) - 180;
    return Math.round((h0 + delta * f + 360) % 360);
  }
  return 212;
}

export function phase(tod) {
  if (tod < 1000) return "Sunrise";
  if (tod < 5000) return "Morning";
  if (tod < 7000) return "Noon";
  if (tod < 11000) return "Afternoon";
  if (tod < 13000) return "Sunset";
  if (tod < 23000) return "Night";
  return "Dawn";
}

export function weather(w) {
  if (w.thundering) return "Thunderstorm";
  return w.raining ? "Rain" : "Clear";
}

const numberFormat = new Intl.NumberFormat();
const compactFormat = new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 });

export function fmtNum(n) {
  return n >= 100_000 ? compactFormat.format(n) : numberFormat.format(n ?? 0);
}

export function fmtPlaytime(ticks) {
  const mins = Math.floor(ticks / 20 / 60);
  return mins >= 60 ? `${Math.floor(mins / 60)}h ${mins % 60}m` : `${mins}m`;
}

export function fmtHours(ticks) {
  return `${fmtNum(Math.floor(ticks / 20 / 3600))} hours`;
}

export function fmtDistance(cm) {
  return cm >= 100_000 ? `${(cm / 100_000).toFixed(1)} km` : `${Math.round(cm / 100)} m`;
}

export function fmtAgo(iso) {
  if (!iso) return "never";
  const secs = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (secs < 60) return "just now";
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

export function fmtWhen(iso) {
  const d = new Date(iso);
  const sameDay = d.toDateString() === new Date().toDateString();
  return d.toLocaleString(undefined, sameDay ? { timeStyle: "short" } : { dateStyle: "medium", timeStyle: "short" });
}

// Minecraft time to a clock: tick 0 is 6:00 AM.
export function mcClock(tod) {
  const h = (Math.floor(tod / 1000) + 6) % 24;
  const m = Math.floor(((tod % 1000) * 60) / 1000);
  return `${h % 12 || 12}:${String(m).padStart(2, "0")} ${h < 12 ? "AM" : "PM"}`;
}

export function capitalize(s = "") {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

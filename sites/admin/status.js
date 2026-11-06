// Pure helpers for admin.ryancaudill.dev: what the page says about the
// server and its controls. No DOM access, so tests/admin can run them
// under Node.

// The status ping's state: Paper, LazyMC, or neither answering.
export const STATES = {
  active: { title: "Online", message: (s) => `${s.version || "Paper"} is running.` },
  sleeping: { title: "Sleeping", message: () => "LazyMC is holding the port. A join wakes Paper." },
  starting: { title: "Starting up", message: () => "Paper is starting." },
  offline: { title: "Offline", message: () => "Neither Paper nor LazyMC answers a status ping." },
  unknown: { title: "Can't reach admin", message: () => "The admin API isn't answering. Retrying…" },
};

// When minecraft.service isn't simply running, systemd knows more than the
// ping: a stopped unit and a crashed one both just look offline to it.
const UNIT_SUMMARY = {
  activating: { state: "starting", title: "Starting…", message: "systemd is starting LazyMC." },
  deactivating: { state: "starting", title: "Stopping…", message: "Paper is saving the world. This can take a few minutes." },
  inactive: { state: "offline", title: "Stopped", message: "LazyMC and Paper are off. Nobody can join until it's started." },
  failed: { state: "offline", title: "Failed", message: "systemd gave up on it. Start it again, or check journalctl --user-unit minecraft." },
};

// The one answer to "is the server up?" that the pill, the Overview card
// and the Server card all show, so they never disagree. state is one of
// the [data-state] colors: loading, active, sleeping, starting, offline or
// unknown.
export function summary(s) {
  if (!s) return { state: "loading", title: "Checking…", message: "One moment." };
  const unit = UNIT_SUMMARY[s.unit?.active];
  if (unit) return unit;
  const state = s.state in STATES ? s.state : "unknown";
  return { state, title: STATES[state].title, message: STATES[state].message(s) };
}

// The Server card's line under the title: the summary's, or with the unit
// running, how LazyMC and Paper are doing. When systemd can't be read, the
// hint under the buttons says so.
export function unitMessage(s) {
  if (!s) return "One moment.";
  if (s.unit?.active !== "active") return summary(s).message;
  switch (s.state) {
    case "active":
      return "LazyMC and Paper are up.";
    case "starting":
      return "LazyMC is up and Paper is starting.";
    case "sleeping":
      return "LazyMC is up. Paper sleeps until someone joins.";
  }
  return "systemd says it's running, but LazyMC isn't answering pings yet.";
}

// Which of start, stop, restart and wake can run now.
export function enabledActions(me, s, pending) {
  const can = !!me?.control && !pending;
  const unit = s?.unit?.active;
  return {
    start: can && (unit === "inactive" || unit === "failed"),
    stop: can && unit === "active",
    restart: can && unit === "active",
    // Waking only makes sense while LazyMC holds the port for a sleeping
    // Paper.
    wake: can && unit === "active" && s?.state === "sleeping",
  };
}

// Says why some of the Server card's buttons are off, or "" when nothing
// needs saying (an action in flight has its own note).
export function actionHint(me, s, pending) {
  if (pending) return "";
  if (!me) return "Checking what you can do…";
  if (!me.email) return me.locked || "admin couldn't tell who you are, so the controls are off.";
  if (!me.control) return "admin can't reach systemd on this machine, so the controls are off.";
  if (!s) return "Checking the server…";
  switch (s.unit?.active) {
    case "active":
      return "Start is off while the server runs.";
    case "inactive":
    case "failed":
      return "Stop and Restart are off while the server is stopped.";
    case "activating":
      return "The controls come back once systemd has started it.";
    case "deactivating":
      return "The controls come back once Paper has saved and stopped.";
  }
  return "admin can't read minecraft.service from systemd, so the controls are off.";
}

// Why the console can't run commands, or "" when it can.
export function consoleLock(me) {
  if (!me) return "Checking…";
  if (me.console) return "";
  return me.locked || "The console isn't available.";
}

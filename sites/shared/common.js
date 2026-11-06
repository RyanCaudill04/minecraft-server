// Helpers play and admin share, served at /shared/common.js.

// Paper sends this UUID for players who hide themselves from the list.
export const ANONYMOUS_ID = "00000000-0000-0000-0000-000000000000";

export const $ = (id) => document.getElementById(id);

// A new element with an optional class and text.
export function el(tag, className = "", text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

export async function getJSON(path) {
  const res = await fetch(path, { cache: "no-store" });
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
  return res.json();
}

export function fmtAgo(iso) {
  if (!iso) return "never";
  const secs = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  if (secs < 60) return "just now";
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return `${Math.floor(secs / 86400)}d ago`;
}

// Highlights the nav link of the last section whose top has scrolled above
// a line 30% down the window, or the final one once the page is scrolled
// to the bottom.
export function scrollSpy() {
  const navLinks = [...document.querySelectorAll(".nav a")];
  const sections = [...document.querySelectorAll(".section")];
  const update = () => {
    const line = innerHeight * 0.3;
    const atBottom = innerHeight + scrollY >= document.documentElement.scrollHeight - 2;
    let current = sections[0];
    for (const s of sections) if (s.getBoundingClientRect().top <= line) current = s;
    if (atBottom && scrollY > 0) current = sections.at(-1);
    if (scrollY < 8) current = sections[0];
    for (const a of navLinks) a.classList.toggle("active", a.hash === `#${current.id}`);
  };
  addEventListener("scroll", update, { passive: true });
  addEventListener("resize", update);
  update();
}

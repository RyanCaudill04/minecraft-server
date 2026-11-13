const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const source = fs.readFileSync("sites/play/app.js", "utf8");
const start = source.indexOf("function el(tag, className = \"\", text) {");
const end = source.indexOf("// Online players from the status sample", start);
assert.notEqual(start, -1, "head helper should exist");
assert.notEqual(end, -1, "head helper section should end");

const context = {
  ANONYMOUS_ID: "00000000-0000-0000-0000-000000000000",
  document: { createElement: (tag) => ({ tag, style: {}, replaceWith(node) { this.replacement = node; } }) },
};
vm.runInNewContext(source.slice(start, end), context);

const anonymousFallback = context.head({ id: context.ANONYMOUS_ID }, 32);
assert.equal(anonymousFallback.style.width, "32px");
assert.equal(anonymousFallback.style.height, "32px");

const loadedImage = context.head({ id: "player-id" }, 32);
loadedImage.onerror();
assert.equal(loadedImage.replacement.style.width, "32px");
assert.equal(loadedImage.replacement.style.height, "32px");


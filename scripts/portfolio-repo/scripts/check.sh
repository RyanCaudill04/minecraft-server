#!/usr/bin/env bash
# Static checks for the site: every page has a <title>, local href and src
# values point at files in site/, and site/ has no dotfiles.
#
# Usage: scripts/check.sh [site folder]
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
site=${1:-$root/site}

[ -f "$site/index.html" ] || { echo "check: $site/index.html is missing" >&2; exit 1; }

python3 - "$site" <<'EOF'
import os, sys
from html.parser import HTMLParser
from urllib.parse import urlsplit, unquote

site = os.path.realpath(sys.argv[1])
failures = []

class Page(HTMLParser):
    def __init__(self):
        super().__init__()
        self.title = False
        self.links = []

    def handle_starttag(self, tag, attrs):
        if tag == "title":
            self.title = True
        for name, value in attrs:
            if name in ("href", "src") and value:
                self.links.append(value)

for folder, dirs, files in os.walk(site):
    for name in dirs + files:
        if name.startswith("."):
            failures.append(f"{os.path.relpath(os.path.join(folder, name), site)}: dotfiles aren't served")
    for name in files:
        if not name.endswith(".html"):
            continue
        path = os.path.join(folder, name)
        rel = os.path.relpath(path, site)
        page = Page()
        with open(path, encoding="utf-8") as f:
            page.feed(f.read())
        if not page.title:
            failures.append(f"{rel}: no <title>")
        for link in page.links:
            url = urlsplit(link)
            if url.scheme or url.netloc or not url.path:
                continue  # another site, mailto:, or a #fragment
            if url.path.startswith("/"):
                target = os.path.join(site, unquote(url.path).lstrip("/"))
            else:
                target = os.path.join(folder, unquote(url.path))
            target = os.path.realpath(target)
            if os.path.isdir(target):
                target = os.path.join(target, "index.html")
            if not target.startswith(site + os.sep) or not os.path.isfile(target):
                failures.append(f"{rel}: {link} doesn't point at a file in the site")

for failure in failures:
    print(f"check: {failure}", file=sys.stderr)
if failures:
    sys.exit(1)
print("check: site OK")
EOF

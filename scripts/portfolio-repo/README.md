# Portfolio

The site behind `ryancaudill.dev`. It's plain static files in [`site/`](site/),
served from the same Raspberry Pi as the Minecraft server, behind its
Cloudflare Tunnel.

It used to live in
[RyanCaudill04/minecraft-server](https://github.com/RyanCaudill04/minecraft-server)
as `sites/portfolio`. It moved here because it shares no code with the server
or its apps (RyanCaudill04/minecraft-server#29 and #85 there). The history
of `site/` came along.

## Layout

| Path | What it is |
|---|---|
| `site/` | Everything that's served. `site/index.html` is the home page. |
| `scripts/check.sh` | The checks CI runs. |
| `scripts/build-release.sh` | Packs `site/` into the release tarball. |

## Working on it

Open `site/index.html` in a browser, or serve the folder:

```bash
python3 -m http.server -d site 8080
```

Before pushing, run the same checks as CI:

```bash
scripts/check.sh
```

They check that every page has a `<title>`, that local links and `src`s
point at files that exist, and that `site/` has no dotfiles, which the
server would refuse to serve anyway.

## Deploys

On the Pi, `serve` (from the minecraft-server repo) serves the current
release's `site/` on `127.0.0.1:8080` as `site@portfolio.service`, and the
tunnel sends `ryancaudill.dev` there.

Releases are built by `scripts/build-release.sh <tag> <folder>`:
`portfolio.tar.gz` holds `site/` and a `VERSION` file, next to a
`.sha256` checksum. CI builds one on every run to check it works.

Publishing releases and installing them on the Pi comes with the release
tracks work in minecraft-server (#79): mc-agent gets a `portfolio` track
pointed at this repo, and installs its newest release the way it installs
the server's. Until then the Pi keeps serving the copy in minecraft-server.

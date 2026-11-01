# Minecraft Server

Configuration, launcher files and web apps for the Raspberry Pi Minecraft
server. The repo holds three things:

- the Paper server's configuration, plugins and launcher (`start.sh`,
  `lazymc.toml`, `config/`, `plugins/`),
- the Go web apps under [`web/`](web) and their pages under [`sites/`](sites),
- the deploy files under [`deploy/`](deploy) and the CI/CD pipeline in
  [`.github/workflows/ci.yml`](.github/workflows/ci.yml).

## Connection details

Connection addresses, account names, and router details are deliberately kept out of this public repository. Authorized users can find them in the ignored `connection.private.md` file on the server-admin machine.

Minecraft Java Edition uses the standard TCP port `25565`. If remote players need access, forward only that port to the server's private LAN address and reserve that address in the router's DHCP settings. Do not forward SSH to the public internet; use SSH keys over the local network or a VPN for administration.

## Repository policy

Commit server configuration, launcher scripts, and plugin configuration. Do not commit generated worlds, logs, caches, downloaded runtime libraries, credentials, private keys, or newly downloaded JAR files. The existing checked-in JARs (`server.jar` and the plugin jars) are retained for the current deployment; newly downloaded JARs are ignored.

Files the server generates are not tracked either. That includes the Paper
jar under `versions/` and spark's `plugins/spark/tmp/` folder, which spark
recreates on start.

Before enabling services such as RCON or the Paper management server, keep their passwords and secrets out of Git. Use a local, ignored override or an environment-specific secret store instead.

## Paper server files

`start.sh` needs `versions/26.2/paper-26.2.jar` and `libraries/`. Neither is
in Git. `server.jar` is the Paperclip launcher, and running it once patches
and writes both. Minecraft 26.1 and newer needs Java 25 or above. From a fresh
clone, in the server folder:

```bash
java -Dpaperclip.patchonly=true -jar server.jar
```

`-Dpaperclip.patchonly=true` writes the files and exits instead of starting
the server. Copy `server.properties.example` to `server.properties` before
the first real start.

A clone made before these files were untracked deletes them on its next
`git pull`. Copy them aside first, or run the command above again afterwards.

## LazyMC

[`lazymc.toml`](lazymc.toml) puts the Paper server to sleep when it has no
players and wakes it when somebody connects. LazyMC listens publicly on TCP
port `25565`; while it is running, Paper is automatically bound privately to
`127.0.0.1:25566`. Keep the router forwarding TCP `25565` only.

On the Raspberry Pi, install the Linux ARM64 LazyMC release binary in this
directory as `lazymc`, then start it from the server directory:

```bash
chmod +x ./lazymc
./lazymc --help
./lazymc start
```

Do not run `start.sh` while LazyMC is running; LazyMC launches it itself. The
first connection wakes Paper. If Paper takes more than 25 seconds to start,
the player will be asked to reconnect once it is ready. Test a complete
start-and-idle-stop cycle while watching LazyMC's output before treating this
as a production deployment.

On the Pi, LazyMC runs as the systemd user unit
[`deploy/systemd/minecraft.service`](deploy/systemd/minecraft.service). That
unit is installed by hand from the git checkout in `~/minecraft_server` (the
steps are in the file), not by mc-agent.

## Web apps

Each app is a small Go server in `web/cmd/<name>`. It listens on loopback
only. Cloudflare Tunnel carries public traffic to it (see
[Outside setup](#outside-setup)).

| App | Command | Site | Address | What it serves |
| --- | --- | --- | --- | --- |
| play | [`web/cmd/play`](web/cmd/play) | play.ryancaudill.dev | `127.0.0.1:8082` | The player dashboard ([`sites/play`](sites/play)) and `GET /api/status`, `/api/world`, `/api/players` |
| admin | [`web/cmd/admin`](web/cmd/admin) | admin.ryancaudill.dev | `127.0.0.1:8081` | The admin page ([`sites/admin`](sites/admin)) and `GET /api/status`, `/api/deploy`. Read-only, behind Cloudflare Access |
| portfolio | [`web/cmd/serve`](web/cmd/serve) | ryancaudill.dev | `127.0.0.1:8080` | The static [`sites/portfolio`](sites/portfolio) folder |
| mc-agent | [`web/cmd/mc-agent`](web/cmd/mc-agent) | none | none | Installs releases on the Pi (see [Releases](#releases-and-deploys)) |
| mc-players | [`web/cmd/mc-players`](web/cmd/mc-players) | none | none | A command-line tool that prints who is online. It isn't part of a release |

- **Server status** comes from status pings, so checking it never wakes the
  server. The apps ping Paper on `127.0.0.1:25566` first, then LazyMC on
  `127.0.0.1:25565`. Paper answering means awake, and LazyMC answering means
  sleeping or starting.
- **World and player data** in play come from files in the server folder
  (`usercache.json` and `world/`), so they work while the server sleeps.
- **Deploy data** in admin comes from `~/apps`, the folder mc-agent installs
  releases into: the active tag, its release notes, and when it was built and
  applied.
- **Shared files.** `sites/shared/` is served at `/shared/` by play and admin.
- **Static sites.** `serve` runs from the template unit
  [`site@.service`](deploy/systemd/site@.service). The instance name is the
  site, and its address is in `deploy/sites/<site>.env`. play's address is
  set in `play.service`; nothing reads `deploy/sites/play.env` today.
- **Caching.** Every static file is served with `Cache-Control: no-cache`, and
  the API with `no-store`, so a new release shows up without purging
  Cloudflare's cache.

Shared Go code is in `web/internal/`: `site` (static files and JSON),
`mcstatus` and `ping` (status pings), `world` and `nbt` (world files), and
`deploy` (mc-agent's install and rollback).

### Running locally

```bash
cd web
go vet ./...
go test -race ./...

# Run play against a local server folder (it reads as offline if nothing
# listens on the Paper and LazyMC ports).
go run ./cmd/play -dir ../sites/play -shared ../sites/shared -server .. -addr 127.0.0.1:8082
go run ./cmd/admin -dir ../sites/admin -shared ../sites/shared -addr 127.0.0.1:8081
go run ./cmd/serve -dir ../sites/portfolio -addr 127.0.0.1:8080
```

The script tests run from the repo root:

```bash
scripts/ci-changes_test.sh
scripts/release-notes_test.sh
```

## CI/CD

[`ci.yml`](.github/workflows/ci.yml) runs on every pull request, every push
to `main`, and by hand.

1. **changes** lists the changed files and
   [`scripts/ci-changes.sh`](scripts/ci-changes.sh) turns them into scopes. On
   a pull request it compares with the merge base. On `main` it compares with
   the newest `r<N>` release tag, so a failed run's changes are picked up by
   the next one.
2. **Scoped jobs** run only when their scope changed:

   | Scope | Turned on by | Checks |
   | --- | --- | --- |
   | go | always | `gofmt`, `go vet`, `go test -race`, an arm64 build |
   | play | `web/cmd/play`, `sites/play`, shared code | JS syntax, smoke test of the built server |
   | admin | `web/cmd/admin`, `web/cmd/serve`, `sites/admin`, `sites/portfolio`, shared code | JS syntax, smoke tests of admin and each static site |
   | mc_server | server configs, jars, `start.sh`, `config/`, `plugins/` | `shellcheck start.sh`, `lazymc.toml` addresses, no secrets in `server.properties.example` |
   | infra | `deploy/`, `scripts/`, `.github/` | actionlint, shellcheck, the script tests, a test release whose units `systemd-analyze` verifies |

   Any change under `.github/` turns every scope on.
3. **ci-ok** passes when every job passed or was skipped. It is the one check
   branch protection requires.
4. **release** runs on `main` after ci-ok, when something that ships changed
   (`web/`, `sites/`, `deploy/`, `scripts/build-release.sh` or the workflows),
   or on a manual run. It:
   - picks the next tag, `r<N+1>`,
   - writes release notes: [`scripts/release-changes.sh`](scripts/release-changes.sh)
     lists the PRs merged since the last tag, and
     [`scripts/release-notes.sh`](scripts/release-notes.sh) turns them into
     `notes.json`. A PR labeled `enhancement` is a feature, `bug` is a fix,
     and anything else is other,
   - builds the release with [`scripts/build-release.sh`](scripts/build-release.sh),
   - publishes it as a GitHub Release with `release.tar.gz` and
     `release.tar.gz.sha256`.

To build a release by hand (it needs Go, and `jq` if you pass notes):

```bash
scripts/build-release.sh <tag> <output folder> [notes.json]
```

The tarball holds the arm64 binaries in `bin/` (play, admin, serve,
mc-agent), `sites/`, `deploy/systemd/`, `deploy/sites/`,
`deploy/release.json`, `deploy/notes.json` when notes were given, and
`VERSION`.

## Releases and deploys

mc-agent runs on the Pi from
[`mc-agent.timer`](deploy/systemd/mc-agent.timer), every 2 minutes. Each run
of `mc-agent deploy`:

1. Asks GitHub for the newest release. It stops if that release is already
   active, or if it failed before (recorded in `~/apps/failed`).
2. Downloads it, checks the SHA-256, and unpacks it into
   `~/apps/releases/<tag>`.
3. Stops, disables and unlinks units the previous release ran that the new
   one doesn't list.
4. Points the `~/apps/current` symlink at the new release and records the
   switch in `~/apps/active.json`.
5. Links each unit in [`deploy/release.json`](deploy/release.json) into
   `~/.config/systemd/user` through `current`, reloads systemd, enables and
   restarts the listed services, and enables the timers.
6. Waits for every health URL in `release.json` to answer.

If any step after the switch fails, mc-agent switches back to the previous
release and writes the tag to `~/apps/failed`, so it isn't retried. Publish a
new release to try again. It keeps the 5 newest releases and removes older
ones. It never touches units installed by hand, such as `minecraft.service`.

Other commands, run on the Pi:

```bash
~/apps/current/bin/mc-agent status     # the agent's version and the active release
~/apps/current/bin/mc-agent rollback   # activate the newest other installed release
~/apps/current/bin/mc-agent version
journalctl --user -u mc-agent          # deploy logs
```

A manual rollback only lasts until the timer's next run. That run sees the
newest release isn't active and installs it again, unless its tag is in
`~/apps/failed`. To stay on the older release, stop the timer first
(`systemctl --user stop mc-agent.timer`) or write the newer tag to
`~/apps/failed`.

### First install on a new Pi

The mc-agent unit runs `~/apps/current/bin/mc-agent`, so the first release has
to be installed by an agent run some other way. Build it on the Mac and run
it once on the Pi:

```bash
cd web && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o ../bin/mc-agent ./cmd/mc-agent
# copy bin/mc-agent to the Pi, then on the Pi:
./mc-agent deploy
```

That installs the newest release, which links and enables everything in
`release.json`, including `mc-agent.timer`. After that the timer keeps the Pi
up to date.

User units only start at boot without a login if lingering is on for that
user (`loginctl enable-linger <user>`). TODO (owner): confirm lingering is
how the Pi is set up.

A fork deploys from its own repo with `mc-agent -repo <owner>/<repo> deploy`.
The unit doesn't pass `-repo`, so a fork also needs to change the default in
`web/cmd/mc-agent/main.go` or the unit.

## Outside setup

Some of the setup lives outside this repo, in the Cloudflare dashboard, the
router and GitHub settings. This is a checklist for rebuilding it. Use
placeholders such as `<your-domain>` and keep the real values in
`connection.private.md`.

### Cloudflare Tunnel

The web apps listen on loopback, and a Cloudflare Tunnel run by `cloudflared`
on the Pi carries traffic for each hostname to them. Nothing on the router is
opened for the websites.

1. Install `cloudflared` on the Pi and run it as a system service.
2. Create a tunnel and map each hostname to its local address:

   | Hostname | Service |
   | --- | --- |
   | `<your-domain>` | `http://127.0.0.1:8080` (portfolio) |
   | `admin.<your-domain>` | `http://127.0.0.1:8081` (admin) |
   | `play.<your-domain>` | `http://127.0.0.1:8082` (play) |

   The addresses come from the units in `deploy/systemd/` and the env files in
   `deploy/sites/`. If you change one, change the tunnel too.

TODO (owner):
- How `cloudflared` is installed and run on the Pi (package, system service,
  and whether the tunnel is managed in the dashboard or by a local config
  file).
- A tracked `deploy/cloudflared/config.example.yml` (#70) doesn't exist yet.

### Cloudflare Access

admin.ryancaudill.dev is behind Cloudflare Access. The admin app itself has no
login and is read-only.

1. In Zero Trust, add a self-hosted application for `admin.<your-domain>`.
2. Add a policy that allows only the owner.

TODO (owner):
- The policy's rules and login method.
- Where the team domain (`<team>.cloudflareaccess.com`) and application AUD
  will go. `secrets.env` (#71) doesn't exist in the repo yet, and nothing in
  the code checks the Access token today.

### DNS

- The website hostnames are routed by the tunnel.
- Minecraft can't go through the tunnel, because it's raw TCP on `25565`, not
  HTTP. It needs a **DNS-only** (grey cloud) `A` record pointing at the home
  connection's public IP, `<mc-hostname>.<your-domain>`.

TODO (owner):
- The Minecraft hostname.
- Dynamic DNS (#19): how the `A` record follows a changing home IP, if it
  does.

### Router

- Forward TCP `25565` only, to the Pi's LAN address. LazyMC listens on it.
- Reserve the Pi's address in DHCP so the forward keeps working.
- Never forward SSH.

TODO (owner): the router model and where these settings are, kept in
`connection.private.md`.

### GitHub

- Actions must be enabled (on a fork they're off until you turn them on).
- The release job needs `contents: write` and `pull-requests: read`, which
  `ci.yml` asks for. It runs in the `production` environment.
- Require the `ci-ok` check on `main`.

TODO (owner): the rulesets on `main` (#7) and any protection rules on the
`production` environment.

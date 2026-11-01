# Minecraft Server

Configuration and launcher files for the Raspberry Pi Minecraft server.

## Connection details

Connection addresses, account names, and router details are deliberately kept out of this public repository. Authorized users can find them in the ignored `connection.private.md` file on the server-admin machine.

Minecraft Java Edition uses the standard TCP port `25565`. If remote players need access, forward only that port to the server's private LAN address and reserve that address in the router's DHCP settings. Do not forward SSH to the public internet; use SSH keys over the local network or a VPN for administration.

## Repository policy

Commit server configuration, launcher scripts, and plugin configuration. Do not commit generated worlds, logs, caches, downloaded runtime libraries, credentials, private keys, or newly downloaded JAR files. The existing checked-in JARs are retained for the current deployment; newly downloaded JARs are ignored.

Before enabling services such as RCON or the Paper management server, keep their passwords and secrets out of Git. Use a local, ignored override or an environment-specific secret store instead.

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

## World map (squaremap)

[squaremap](https://github.com/jpenilla/squaremap) renders a top-down map
of the world to PNG tiles on disk while Paper is awake, and play serves
them read-only at `https://play.ryancaudill.dev/map/`. The map keeps
working while Paper sleeps, because play only reads files. squaremap's own
web server is off ([`plugins/squaremap/config.yml`](plugins/squaremap/config.yml)),
so nothing new listens on the Pi.

- **CPU:** one render thread. Changed chunks are re-rendered in batches of
  at most 256 every 30 seconds. A first full render runs only when you ask
  for it (`squaremap fullrender world` in the console) and keeps one of the
  Pi's four cores busy until it finishes: minutes for a few thousand blocks
  across, longer for a pre-generated world.
- **Disk:** about one 512×512 PNG per region (512×512 blocks) at full zoom,
  plus a third more for the zoomed-out levels. Rough estimate: 100–250 KB
  per region, so a 5,000-block-wide explored area (~100 regions) is about
  15–35 MB and a 20,000-block-wide one (~1,600 regions) is about 200–550 MB.
  `du -sh plugins/squaremap/web/tiles` shows the real number.
- **Area:** `visibility-limits` in the config renders only a circle of
  radius 5,000 blocks around 0,0 (~300 regions, at most about 40–100 MB).
  Without it, a full render covers every generated chunk, including a whole
  Chunky pregen. Move the center to spawn, and widen it as people build
  farther out.
- **Privacy:** every build inside the limit is visible to anyone with the
  URL. Live player positions are off in squaremap's config, and play always
  answers `tiles/players.json` with an empty list. Showing positions would
  first need a filter for players who hide from the server list (#113) and
  a CSP that allows squaremap's player heads.
- To reset the map, stop Paper and delete `plugins/squaremap/web/tiles`.

## Plugins

[`plugins/manifest.json`](plugins/manifest.json) pins each plugin jar by
file name and SHA-256, with the URL it was downloaded from. Admin's
Plugins section shows each installed jar as pinned, changed, unpinned,
missing or invalid. When you update a plugin, update its manifest entry in
the same PR; `go test ./internal/plugins` checks the checked-in jars
against it.

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

## Day-to-day control from the Mac

[`scripts/mc`](scripts/mc) wraps `ssh mc-pi` (set `MC_HOST` for another
host). Commands that only look run right away; anything that changes the
Pi asks first, or takes `--yes`.

```bash
scripts/mc status            # services, timers, active release, server state
scripts/mc logs play -f      # follow a unit's journal (default: minecraft)
scripts/mc restart           # restart LazyMC and Paper
scripts/mc deploy            # install the newest release now
scripts/mc pause && scripts/mc rollback   # roll back and keep it
scripts/mc preview           # run play locally against a copy of the world
```

`scripts/mc --help` lists everything.

## Dynamic DNS

`ddns.timer` (shipped in every release) checks the public IP every five
minutes and updates the Minecraft address's Cloudflare A record when it
changes. It does nothing until these are set in
`~/.config/minecraft/secrets.env` (mode `0600`) on the Pi:

```bash
# API token with Zone > DNS > Edit on this zone only
CLOUDFLARE_DNS_TOKEN=
# From the zone's overview page in Cloudflare
CLOUDFLARE_ZONE_ID=
# The existing A record, e.g. mc.example.com
DDNS_RECORD=
```

systemd doesn't strip comments at the end of a line, so keep them on their
own lines.

Check it with `scripts/mc logs ddns`, or try it without changing anything:
`set -a; . ~/.config/minecraft/secrets.env; ~/apps/current/bin/ddns -dry-run`.

# Minecraft Server

Configuration and launcher files for the Raspberry Pi Minecraft server.

## Connection details

Connection addresses, account names, and router details are deliberately kept out of this public repository. Authorized users can find them in the ignored `connection.private.md` file on the server-admin machine.

Minecraft Java Edition uses the standard TCP port `25565`. If remote players need access, forward only that port to the server's private LAN address and reserve that address in the router's DHCP settings. Do not forward SSH to the public internet; use SSH keys over the local network or a VPN for administration.

## Repository policy

Commit server configuration, launcher scripts, and plugin configuration. Do not commit generated worlds, logs, caches, downloaded runtime libraries, credentials, private keys, or newly downloaded JAR files. The existing checked-in JARs are retained for the current deployment; newly downloaded JARs are ignored.

Before enabling services such as RCON or the Paper management server, keep their passwords and secrets out of Git. Use a local, ignored override or an environment-specific secret store instead.

## Privacy

play.ryancaudill.dev is public and has no login, so it follows one rule:
**a player is shown by name only if the server list shows them by name.**
That is Minecraft's own "Allow Server Listings" setting (Options → Online).
With it off, Paper sends "Anonymous Player" in place of the name and UUID,
and play treats that player as hidden everywhere. Anything play can't confirm
counts as hidden.

play learns who is listed from status pings to Paper, every 15 seconds and on
page loads (pings never wake the server). It remembers each listed player's
UUID, name and the last time they were listed, in
`~/.local/state/play/listed.json` on the Pi. A listed player stays public
while their saved data is no newer than that time plus 30 seconds (for the
save Paper makes as they log out). If they play without being listed, for
example after turning the setting off, their data gets newer and they drop
off the page until they are listed again. Their old totals were already
public; their new playtime and last-seen time are not shown.

| Data | Where | Public players | Hidden players |
| --- | --- | --- | --- |
| State (active, sleeping, starting, offline), version, max players | `/api/status` | Shown | Shown |
| Online count | `/api/status` | Counted | Counted, never named |
| Online names and UUIDs | `/api/status` | Shown (as the server list shows them) | Left out |
| World facts: name, version, difficulty, day, time, weather, spawn, age, last save | `/api/world` | Shown | Shown |
| How many players have joined | `/api/world` (`players`) | Counted | Counted |
| Name, UUID and skin head (loaded from mc-heads.net by UUID) | `/api/players`, Players, Leaderboards, player sheet | Shown | Left out |
| Last-seen time | `/api/players`, "… was on 2h ago", Players, player sheet | Shown | Left out |
| Playtime, deaths, mob and player kills, blocks mined, distance, jumps, fish, animals bred, advancements | `/api/players`, Leaderboards, player sheet | Shown | Left out |

Never shown for anyone: chat, commands, IP addresses, coordinates (other
than world spawn), inventories, and anything from `ops.json`, `whitelist.json`
or the ban lists.

Known limits:
- The online count is public for everyone. Paper answers the same ping on
  port 25565, so anyone can already see it in their server list.
- If a listed player logs out and rejoins hidden within 30 seconds, one save
  from the hidden session can show. Waiting a minute avoids it.
- Paper's sample lists at most 12 players. With more online, some listed
  players may briefly drop off the page (hidden, never wrongly shown).

The player lists (`ops.json`, `whitelist.json`, `banned-players.json`,
`banned-ips.json`) are tracked only as empty `[]` files, and history has never
held anything else. Don't commit them with names in them.

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

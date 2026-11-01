# Minecraft Server

Configuration and launcher files for the Raspberry Pi Minecraft server.

## Connection details

Connection addresses, account names, and router details are deliberately kept out of this public repository. Authorized users can find them in the ignored `connection.private.md` file on the server-admin machine.

Minecraft Java Edition uses the standard TCP port `25565`. If remote players need access, forward only that port to the server's private LAN address and reserve that address in the router's DHCP settings. Do not forward SSH to the public internet; use SSH keys over the local network or a VPN for administration.

## Repository policy

Commit server configuration, launcher scripts, and plugin configuration. Do not commit generated worlds, logs, caches, downloaded runtime libraries, credentials, or private keys. The Paper and plugin JARs listed under [Paper and plugins](#paper-and-plugins) are checked in on purpose (add a new one with `git add -f`); any other JAR is ignored.

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

## Paper and plugins

| File | Version | SHA-256 | Source |
|---|---|---|---|
| `server.jar` (Paperclip) | Paper 26.2 build 135 | `ef3dbc68b013deafdbbee3bebf1ba351671ebcda94a8f136809b7532d5ba2fe1` | [PaperMC downloads API](https://fill.papermc.io/v3/projects/paper/versions/26.2/builds/135) |
| `versions/26.2/paper-26.2.jar` | patched from the above | `7c391444fd5f5fb77d193414924e7a16dbfd1cac87a2a6c858a8ea8dd041f44e` | `META-INF/versions.list` in `server.jar` |
| `plugins/ViaVersion-5.12.2.jar` | ViaVersion 5.12.2 | `c0b63badcb0f7d38f4443e06820f2b8142dd4f0d3b0511f9330ff1050430e6b2` | [Hangar](https://hangar.papermc.io/ViaVersion/ViaVersion/versions/5.12.2) |
| `plugins/Chunky-Bukkit-1.5.3.jar` | Chunky 1.5.3 | `530d2c7430a96a39957391b7088be144daa3108f7665896d1c23aa8dd4af32f3` | |

`start.sh` runs `versions/26.2/paper-26.2.jar` with every JAR under
`libraries/` on the classpath. Paperclip (`server.jar`) produces both, and
needs Java 25:

```bash
java -Dpaperclip.patchonly=true -jar server.jar
```

To update Paper within the same Minecraft version: download the newest
`STABLE` build from
`https://fill.papermc.io/v3/projects/paper/versions/<version>/builds/latest`,
check its SHA-256 against the API, save it as `server.jar`, run the command
above, check that `versions/26.2/paper-26.2.jar` matches the hash in
`server.jar`'s `META-INF/versions.list`, and commit both. Changing the
Minecraft version is a separate decision: it changes the world format and the
version every client needs.

### Applying a server update on the Pi

Server files don't deploy automatically yet (#15). After a change to Paper,
plugins or server configs is merged:

```bash
systemctl --user start mc-backup   # back up first; check journalctl --user -u mc-backup
systemctl --user stop minecraft
cd ~/minecraft_server && git pull
java -Dpaperclip.patchonly=true -jar server.jar   # refresh libraries/
systemctl --user start minecraft
```

Because `start.sh` puts every JAR under `libraries/` on the classpath, delete
the folder of any library version Paper no longer lists (for example an old
`libraries/io/papermc/paper/paper-api/<build>/`) before starting.

## Backups

`mc-backup.timer` runs [`deploy/backup/mc-backup.sh`](deploy/backup/mc-backup.sh)
every night at about 04:30. It ships in the release, so mc-agent installs and
updates it like the web apps. It never starts, stops or waits on
`minecraft.service`. Each run writes
`~/backups/minecraft/world-YYYYmmdd-HHMMSS.tar.gz` with a `.sha256` file next
to it, checks the archive, and keeps the newest backup of each of the last 7
days and of each of the last 4 weeks. An archive holds `world`,
`world_nether`, `world_the_end` and the runtime files git doesn't track
(`server.properties`, `ops.json`, `whitelist.json`, the ban lists and
`usercache.json`).

- **Paper asleep** (the usual case at night): the world is copied as-is.
- **Paper awake, RCON on:** saving is paused with `save-off` and
  `save-all flush` during the copy, then turned back on with `save-on`.
- **Paper awake, RCON off:** the backup waits up to 30 minutes for Paper to
  sleep, then fails rather than copy files Paper is writing.

To let backups run while people are playing, turn on RCON in the Pi's
`server.properties` (never in `server.properties.example`). It binds to
`server-ip`, which LazyMC sets to `127.0.0.1`. Don't forward its port.

```properties
enable-rcon=true
rcon.port=25575
rcon.password=<long random letters and digits>
```

Settings such as retention and an off-Pi copy go in `~/.config/mc-backup.env`;
the script's header lists them all. For example:

```bash
MC_BACKUP_KEEP_DAILY=7
MC_BACKUP_KEEP_WEEKLY=8
MC_BACKUP_COPY_TO=user@nas:backups/minecraft/
```

Run one now with `systemctl --user start mc-backup` and read its log with
`journalctl --user -u mc-backup`.

### Restoring

```bash
systemctl --user stop minecraft
cd ~/backups/minecraft && sha256sum -c world-YYYYmmdd-HHMMSS.tar.gz.sha256
mkdir ~/restore && tar -xzf world-YYYYmmdd-HHMMSS.tar.gz -C ~/restore
cd ~/minecraft_server
mkdir ~/world-before-restore && mv world world_nether world_the_end ~/world-before-restore/
mv ~/restore/world ~/restore/world_nether ~/restore/world_the_end .
systemctl --user start minecraft
```

Only move back what you need: `~/restore` also has the backed-up
`server.properties` and player lists. Delete `~/world-before-restore` once the
restored world looks right.

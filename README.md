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

## Pinned server versions

[`deploy/server.json`](deploy/server.json) pins the Minecraft version, the
Paper build (Paperclip `server.jar`), LazyMC and each plugin, each with its
download URL and SHA-256. To fill a server folder from it:

```bash
scripts/fetch-server.sh [--patch] <server folder>
```

It downloads what's missing, checks every SHA-256, and stops on a file that
doesn't match the manifest. `--patch` also runs Paperclip to make
`versions/` and `libraries/` (needs Java 25). To upgrade, change the
manifest in a PR, then run the script with `--replace` on the server. CI
checks that the tracked jars match the manifest.

## Server folder

`scripts/migrate-server-dir.sh` moves the world and other runtime files out
of the checkout into their own server folder (#73). It's a dry run unless
given `--apply`, and refuses to run while the server is up. On the Pi, the
plan is to keep the server folder at `~/minecraft_server` and move the
checkout:

```bash
scripts/migrate-server-dir.sh --move-checkout-to ~/src/minecraft-server
```

## mc-players

`web/cmd/mc-players` is a small dev tool, not part of a release. It prints
the online count and player names with a status ping, which doesn't wake
LazyMC:

```bash
cd web && go run ./cmd/mc-players -addr <host>:25565
```

The Paper boot test in CI (#104) uses it. Changing it doesn't publish a
release.

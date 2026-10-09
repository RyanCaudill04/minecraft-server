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

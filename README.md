# Minecraft Server

Configuration and launcher files for the Raspberry Pi Minecraft server.

## Connection details

Connection addresses, account names, and router details are deliberately kept out of this public repository. Authorized users can find them in the ignored `connection.private.md` file on the server-admin machine.

Minecraft Java Edition uses the standard TCP port `25565`. If remote players need access, forward only that port to the server's private LAN address and reserve that address in the router's DHCP settings. Do not forward SSH to the public internet; use SSH keys over the local network or a VPN for administration.

## Repository policy

Commit server configuration, launcher scripts, and plugin configuration. Do not commit generated worlds, logs, caches, downloaded runtime libraries, credentials, private keys, or newly downloaded JAR files. The existing checked-in JARs are retained for the current deployment; newly downloaded JARs are ignored.

Before enabling services such as RCON or the Paper management server, keep their passwords and secrets out of Git. Use a local, ignored override or an environment-specific secret store instead.

#!/bin/bash
# Starts Paper in the mc-server container. LazyMC (entrypoint.sh) runs this
# on demand from the server folder, /server; never run it while LazyMC is
# running.
#
# The patched Paper jar and its libraries are in the image (Dockerfile).
# MC_HEAP sets the heap, such as 10G or 6144M; compose.yaml passes it from
# ~/.config/minecraft/host.env, and the default is 10G.
set -euo pipefail

home=/opt/minecraft
heap=${MC_HEAP:-10G}
if [[ ! $heap =~ ^[1-9][0-9]*[MG]$ ]]; then
  echo "start.sh: MC_HEAP must be a size such as 10G or 6144M, got '$heap'" >&2
  exit 1
fi
if [[ ! -f $home/paper.jar || ! -d $home/libraries ]]; then
  echo "start.sh: $home/paper.jar or $home/libraries is missing; the image is broken" >&2
  exit 1
fi

classpath=$home/paper.jar
while IFS= read -r -d '' library; do
  classpath="$classpath:$library"
done < <(find "$home/libraries" -type f -name '*.jar' -print0 | sort -z)

exec java "-Xms$heap" "-Xmx$heap" -cp "$classpath" org.bukkit.craftbukkit.Main --nogui

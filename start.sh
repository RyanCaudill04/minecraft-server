#!/bin/bash

set -euo pipefail

# HEAP comes from host.env through minecraft.service and LazyMC (see
# deploy/host.example.env).
heap=${HEAP:-10G}
if [[ ! "$heap" =~ ^[1-9][0-9]*[MG]$ ]]; then
  echo "HEAP=$heap is not a size like 4G or 4096M; fix it in ~/.config/minecraft/host.env." >&2
  exit 1
fi

server_jar="versions/26.2/paper-26.2.jar"
libraries_dir="libraries"

if [[ ! -f "$server_jar" || ! -d "$libraries_dir" ]]; then
  echo "Missing $server_jar or $libraries_dir; install the Paper server files first." >&2
  exit 1
fi

classpath="$server_jar"
while IFS= read -r -d '' library; do
  classpath="$classpath:$library"
done < <(find "$libraries_dir" -type f -name '*.jar' -print0)

exec java "-Xms$heap" "-Xmx$heap" -cp "$classpath" org.bukkit.craftbukkit.Main --nogui

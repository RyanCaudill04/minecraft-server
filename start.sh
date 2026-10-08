#!/bin/bash

set -euo pipefail

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

exec java -Xms8G -Xmx12G -cp "$classpath" org.bukkit.craftbukkit.Main --nogui

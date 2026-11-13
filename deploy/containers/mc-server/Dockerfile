# syntax=docker/dockerfile:1
#
# The Minecraft server: Java, LazyMC and the patched Paper jar with its
# libraries, all pinned by deploy/server.json. The world, configs and
# plugins aren't in the image; they're the server folder compose.yaml
# mounts at /server. Build from the repo root:
#
#   docker buildx build -f deploy/containers/mc-server/Dockerfile .
#
# The image is tagged by a hash of its inputs (scripts/images.sh), so a
# release that doesn't change them keeps the same image and doesn't
# restart the server.

FROM eclipse-temurin:25-jre-noble@sha256:a98128a57de944a72bd31fbc79b898edd5a5864d3652071d5bbd2160ab7d7dce AS fetch
ARG TARGETARCH
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl jq \
    && rm -rf /var/lib/apt/lists/*
COPY deploy/server.json /src/deploy/server.json
COPY scripts/fetch-server.sh /src/scripts/fetch-server.sh
# fetch-server.sh checks every download's SHA-256, then runs Paperclip
# (--patch), which writes the patched Paper jar and libraries/ and is
# checked against the manifest too. Plugins live in the server folder, so
# none are fetched here.
RUN echo '{"plugins": []}' > /tmp/no-plugins.json \
    && mkdir /opt/minecraft \
    && /src/scripts/fetch-server.sh --arch "$TARGETARCH" --patch --plugins /tmp/no-plugins.json /opt/minecraft \
    && mv "/opt/minecraft/$(jq -r .paper.patched.path /src/deploy/server.json)" /opt/minecraft/paper.jar

FROM eclipse-temurin:25-jre-noble@sha256:a98128a57de944a72bd31fbc79b898edd5a5864d3652071d5bbd2160ab7d7dce
LABEL org.opencontainers.image.source=https://github.com/RyanCaudill04/minecraft-server
LABEL org.opencontainers.image.description="The Minecraft server: LazyMC and Paper"
COPY --from=fetch /opt/minecraft/lazymc /opt/minecraft/paper.jar /opt/minecraft/
COPY --from=fetch /opt/minecraft/libraries /opt/minecraft/libraries
COPY deploy/containers/mc-server/entrypoint.sh deploy/containers/mc-server/start.sh deploy/containers/mc-server/lazymc.toml /opt/minecraft/
# compose.yaml runs this as the Pi's user, who has no home in the image.
# Paper writes into the server folder; this is for anything that wants a
# home (Java's preferences, for one).
RUN mkdir -m 1777 /home/minecraft
ENV HOME=/home/minecraft
WORKDIR /server
ENTRYPOINT ["/opt/minecraft/entrypoint.sh"]

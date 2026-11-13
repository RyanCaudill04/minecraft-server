# syntax=docker/dockerfile:1
#
# play and admin, the two web apps, as two targets of one build: the Go
# build is shared, and each image holds one static binary, the site it
# serves and sites/shared. Build from the repo root:
#
#   docker buildx build -f deploy/containers/web/Dockerfile --target play .
#
# deploy/containers/compose.yaml runs them on the Pi, with the folders and
# flags that depend on the host.

FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src/web
COPY web/ ./
# Cross-compiled on the build machine, so an arm64 image doesn't need an
# arm64 builder.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/ ./cmd/play ./cmd/admin

# No shell, no package manager: just the binary and its files. compose.yaml
# runs it as the Pi's user, so files it reads keep their owner.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931 AS base
LABEL org.opencontainers.image.source=https://github.com/RyanCaudill04/minecraft-server
COPY sites/shared /app/sites/shared

FROM base AS play
LABEL org.opencontainers.image.description="play.ryancaudill.dev: server status, world facts and player stats"
COPY --from=build /out/play /app/play
COPY sites/play /app/sites/play
ENTRYPOINT ["/app/play", "-dir", "/app/sites/play", "-shared", "/app/sites/shared"]
CMD ["-server", "/server", "-addr", "127.0.0.1:8082"]

FROM base AS admin
LABEL org.opencontainers.image.description="admin.ryancaudill.dev: live status, server controls and the active release"
COPY --from=build /out/admin /app/admin
COPY sites/admin /app/sites/admin
ENTRYPOINT ["/app/admin", "-dir", "/app/sites/admin", "-shared", "/app/sites/shared"]
CMD ["-server", "/server", "-apps", "/apps", "-backups", "/backups", "-addr", "127.0.0.1:8081"]

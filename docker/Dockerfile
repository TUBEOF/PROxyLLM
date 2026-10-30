# syntax=docker/dockerfile:1

# WebUI stage: build the SPA so every built image embeds the real bundle,
# not the committed dist placeholder. Pinned to the same node major CI and
# the workflows use (26); deps from the lockfile only (npm ci, never npm
# install).
FROM node:26.13.0-alpine AS webui

WORKDIR /src/webui
COPY webui/package.json webui/package-lock.json ./
RUN npm ci

# Self-hosted fonts live in public/fonts (Phase 2, assetsInlineLimit 0).
COPY webui/vite.config.ts webui/index.html webui/tsconfig.json ./
COPY webui/public/ public/
COPY webui/src/ src/
RUN npm run build
# vite build emits dist/ under /src/webui/dist

# Build stage: compile the static CGO-free binary with the WebUI embedded.
FROM golang:1.27.3-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
# The embed package's single Go source must exist in the build context —
# without it the github.com/tubeof/proxyllm/webui import (server.go) has
# no Go files and the build fails.
COPY webui/embed.go webui/
# The embedded SPA comes from the webui stage; the placeholder under
# webui/dist (committed for fresh-clone compiles) is never shipped.
COPY --from=webui /src/webui/dist/ webui/dist/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxyllm ./cmd/proxyllm \
    && mkdir -p /out/data

# Final stage: distroless-style, non-root, read-only-rootfs compatible.
# gcr.io/distroless/static-debian12 is multi-platform (GoReleaser R-2), so
# the same Dockerfile serves both amd64 and arm64 release builds.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/proxyllm /proxyllm
# Pre-create /data owned by the nonroot user (UID 65532) so the binary can
# create its SQLite file; distroless has no shell, so ownership is baked
# in via COPY --chown rather than RUN mkdir/chown.
COPY --from=build --chown=65532:65532 /out/data /data

WORKDIR /
USER nonroot:nonroot

ENV PROXYLLM_STORAGE_PATH=/data/proxyllm.db \
    PROXYLLM_MASTER_KEY_PATH=/data/master.key

EXPOSE 8080
ENTRYPOINT ["/proxyllm"]
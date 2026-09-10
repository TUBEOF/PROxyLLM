# syntax=docker/dockerfile:1

# Build stage: compile the static CGO-free binary.
FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxyllm ./cmd/proxyllm \
    && mkdir -p /out/data

# Final stage: distroless-style, non-root, read-only-rootfs compatible.
# Only writable path needed is /data (bind-mounted).
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
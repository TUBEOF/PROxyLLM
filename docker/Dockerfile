# syntax=docker/dockerfile:1

# Build stage: compile the static CGO-free binary.
FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxyllm ./cmd/proxyllm

# Final stage: distroless-style, non-root, read-only-rootfs compatible.
# Only writable path needed is /data (bind-mounted).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/proxyllm /proxyllm

# Data dir is a bind mount; create the mount point with correct ownership.
VOLUME /data
WORKDIR /
USER nonroot:nonroot

ENV PROXYLLM_STORAGE_PATH=/data/proxyllm.db \
    PROXYLLM_MASTER_KEY_PATH=/data/master.key

EXPOSE 8080
ENTRYPOINT ["/proxyllm"]
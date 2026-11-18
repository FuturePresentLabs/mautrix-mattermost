# syntax=docker/dockerfile:1

# Builds an image that works both as a normal container and as a Shroud
# microVM rootfs. Shroud converts the OCI image with `shroudoci extract` and
# runs the packaged ENTRYPOINT as root inside the guest.
#
# MicroVM-specific assumptions (see deploy/shroud.md):
#   * only /data is persistent; everything else is an ephemeral overlay
#   * /tmp and /var/tmp are tmpfs
#   * config, registration and the SQLite database live under /data
#   * Shroud supplies env and secrets; it does not run a container runtime

# --- build -------------------------------------------------------------------
FROM golang:1.26-alpine AS build

# go-sqlite3 needs a C compiler. -tags goolm keeps libolm out of the build.
RUN apk add --no-cache build-base

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.0.0-dev
ARG TAG=unknown
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN MAUTRIX_VERSION="$(awk '$1 == "maunium.net/go/mautrix" { print $2; exit }' go.mod)" && \
    CGO_ENABLED=1 go build -tags goolm -trimpath \
        -ldflags="-s -w \
            -X main.Version=${VERSION} \
            -X main.Tag=${TAG} \
            -X main.Commit=${COMMIT} \
            -X 'main.BuildTime=${BUILD_TIME}' \
            -X 'maunium.net/go/mautrix.GoModVersion=${MAUTRIX_VERSION}'" \
        -o /out/mautrix-mattermost ./cmd/mautrix-mattermost

# --- runtime ------------------------------------------------------------------
FROM alpine:3.22

# CA certificates are needed for the homeserver (hungryserv/Matrix) and
# Mattermost HTTPS calls. SQLite is compiled into the binary.
RUN apk add --no-cache ca-certificates

COPY --from=build /out/mautrix-mattermost /usr/local/bin/mautrix-mattermost
COPY docker/entrypoint.sh /usr/local/bin/mautrix-mattermost-entrypoint

RUN chmod 0755 /usr/local/bin/mautrix-mattermost /usr/local/bin/mautrix-mattermost-entrypoint && \
    mkdir -p /data/mautrix-mattermost

# Shroud mounts persistent storage under /data (block volumes). Older hosts
# expose a virtiofs volume at /var/lib/registry; the entrypoint detects both.
# Keep the /data path stable: it is the only location that survives a restart.

# Default appservice port (see pkg/connector/connector.go DefaultPort).
EXPOSE 29345

STOPSIGNAL SIGTERM

ENTRYPOINT ["/usr/local/bin/mautrix-mattermost-entrypoint"]

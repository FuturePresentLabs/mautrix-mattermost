# mautrix-mattermost
[![License](https://img.shields.io/github/license/bostrot/mautrix-mattermost.svg)](LICENSE)

A Matrix–[Mattermost](https://mattermost.com) bridge built on the [mautrix bridgev2](https://github.com/mautrix/go) framework.

> **This is the Future Present Labs fork** of [bostrot/mautrix-mattermost](https://github.com/bostrot/mautrix-mattermost). It adds native threading, a proper channel archive/restore/delete lifecycle, and reaction backfill, and ships ready-to-run releases. See [About this fork](#about-this-fork).

Bridges text, media, reactions, edits, deletions, replies, emoji shortcodes, typing notifications, and backfill. Supports Personal Access Token (PAT) and email+password login. All teams, channels, and DMs are bridged automatically on login.

## Requirements

- Go 1.26+ and a C compiler (only for building from source; SQLite needs CGO)
- A Matrix homeserver
- SQLite (default) or PostgreSQL
- A Mattermost server with API v4

## Installation

### Download a release (easiest)

Linux binaries for `amd64` and `arm64` are attached to each [release](https://github.com/FuturePresentLabs/mautrix-mattermost/releases), with a checksum:

```sh
curl -LO https://github.com/FuturePresentLabs/mautrix-mattermost/releases/latest/download/mautrix-mattermost-linux-amd64.tar.gz
curl -LO https://github.com/FuturePresentLabs/mautrix-mattermost/releases/latest/download/mautrix-mattermost-linux-amd64.tar.gz.sha256
sha256sum -c mautrix-mattermost-linux-amd64.tar.gz.sha256
tar xzf mautrix-mattermost-linux-amd64.tar.gz     # produces ./mautrix-mattermost
```

Use `linux-arm64` on ARM machines.

### Build from source

```sh
git clone https://github.com/FuturePresentLabs/mautrix-mattermost.git
cd mautrix-mattermost
./build.sh          # produces ./mautrix-mattermost
```

## Setup

### With Beeper / bbctl

```sh
bbctl config --type bridgev2 sh-mymattermost -o config.yaml
./build.sh && ./mautrix-mattermost -c config.yaml
```

Or let `bbctl` register the bridge and run the binary for you:

```sh
bbctl run --type bridgev2 --no-override-config \
  --custom-startup-command ./mautrix-mattermost sh-mymattermost
```

To start over from a clean slate, `bbctl delete sh-mymattermost` removes the bridge and its rooms on the Beeper side (and the local data directory). It does not touch your Mattermost server.

### Standalone

```sh
# 1. Generate config
./mautrix-mattermost -e -c config.yaml   # or: cp example-config.yaml config.yaml

# 2. Edit config.yaml — set homeserver.address, homeserver.domain,
#    appservice.address, bridge.permissions, database.uri

# 3. Generate registration and add it to your homeserver
./mautrix-mattermost -g -c config.yaml -r registration.yaml

# 4. Run
./mautrix-mattermost -c config.yaml
```

See [docs.mau.fi — Registering appservices](https://docs.mau.fi/bridges/general/registering-appservices.html) for homeserver-specific registration steps.

## Logging in

Send `login` to the bridge bot. Two methods are available:

- **Personal Access Token** — server URL + PAT (recommended; generate one in Mattermost under *Profile → Security → Personal Access Tokens*)
- **Email & password** — server URL + email + password (may be blocked by Cloudflare or disabled by the server admin)

## About this fork

This fork tracks upstream and carries the changes below. They are also open as upstream pull requests, so some or all of them may eventually be merged there.

| Change | Upstream PR |
|---|---|
| Native Matrix threading for Mattermost threads | [#8](https://github.com/bostrot/mautrix-mattermost/pull/8) |
| Channel lifecycle: an archived channel keeps its room, shows Mattermost's own archive/restore message and becomes read-only; a permanently deleted channel removes its room, including one deleted while the bridge was offline | [#9](https://github.com/bostrot/mautrix-mattermost/pull/9) |
| Reactions are backfilled with the messages they belong to | [#10](https://github.com/bostrot/mautrix-mattermost/pull/10) |

How the lifecycle behaves, and what to watch for:

- **Archive** raises the room's `events_default` (and any explicit message overrides) to power level 50 so members cannot post. It never lowers a stricter setting, restores only what it changed, and leaves an administrator's own edits alone.
- **Delete** removes the room only after the server answers with a canonical 404 for an authenticated session. Anything uncertain (a 403, a server error, a dropped connection) keeps the room.
- Matrix has no compare-and-set for room state, so an administrator edit made at the exact moment of an archive or restore can be overwritten.
- The deferred delete check requires `async_events: false` in the bridge config. `bbctl` generates it that way; if you set it to `true` the bridge keeps rooms instead of deleting them.
- The bridge polls the channel list about every 30 seconds to catch events the Mattermost websocket missed.

Releases are tagged `vX.Y.Z-fpl.N`. The `main` branch mirrors upstream; this branch (`fpl-main`) is the one to use.

## Development

```sh
# Live reload
air

# Tests
go test -tags goolm ./...
```

The `air.toml` rebuilds with `-tags goolm` (pure-Go olm, no C dependency).

### Structure

```
cmd/mautrix-mattermost/   entry point
pkg/connector/            bridgev2 NetworkConnector + NetworkAPI
pkg/mattermost/           Mattermost HTTP REST + WebSocket client
example-config.yaml       config template
build.sh                  release build
Dockerfile                production image
```

## License

[Apache 2.0](LICENSE)

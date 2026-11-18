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

Binaries are attached to each [release](https://github.com/FuturePresentLabs/mautrix-mattermost/releases) with a checksum, for these platforms:

| Platform | File |
|---|---|
| Linux x86_64 | `mautrix-mattermost-linux-amd64.tar.gz` |
| Linux ARM64 | `mautrix-mattermost-linux-arm64.tar.gz` |
| macOS Apple Silicon | `mautrix-mattermost-darwin-arm64.tar.gz` |
| macOS Intel | `mautrix-mattermost-darwin-amd64.tar.gz` |

```sh
# pick the file for your platform
file=mautrix-mattermost-linux-amd64.tar.gz
curl -LO "https://github.com/FuturePresentLabs/mautrix-mattermost/releases/latest/download/$file"
curl -LO "https://github.com/FuturePresentLabs/mautrix-mattermost/releases/latest/download/$file.sha256"

# verify (either form works)
shasum -a 256 -c "$file.sha256"      # macOS
sha256sum -c "$file.sha256"          # Linux

tar xzf "$file"                      # produces ./mautrix-mattermost
./mautrix-mattermost --version
```

On macOS the binary is not notarized, so Gatekeeper may quarantine it. If it refuses to run, clear the quarantine flag once:

```sh
xattr -d com.apple.quarantine ./mautrix-mattermost
```

### Build from source

```sh
git clone https://github.com/FuturePresentLabs/mautrix-mattermost.git
cd mautrix-mattermost
./build.sh          # produces ./mautrix-mattermost
```

`build.sh` takes no flags: anything after it is passed to `go build` after the package path, where `-o` is read as a package name. The output is always `./mautrix-mattermost`.

Building for a different architecture from a Mac needs the matching SDK or toolchain, for example an Intel binary from an Apple Silicon Mac:

```sh
GOARCH=amd64 CGO_ENABLED=1 ./build.sh
```

### Docker or a Shroud microVM

The repository ships a `Dockerfile` that is also a valid Shroud rootfs: config,
registration and the SQLite database live under `/data/mautrix-mattermost`, and
the entrypoint runs without an init system. See [deploy/shroud.md](deploy/shroud.md)
for the rootfs build, persistent volume layout, and deployment request.

```sh
docker build -t mautrix-mattermost .
```

## Setup

### With Beeper / bbctl

```sh
bbctl config --type bridgev2 sh-mattermost -o config.yaml
./build.sh && ./mautrix-mattermost -c config.yaml
```

Or let `bbctl` register the bridge and run the binary for you:

```sh
bbctl run --type bridgev2 --no-override-config \
  --custom-startup-command ./mautrix-mattermost sh-mattermost
```

The bridge name (`sh-mattermost` above) is arbitrary: `sh-` marks a self-hosted bridge in Beeper. Pick any name, but keep it the same in `bbctl config`, `bbctl run` and `bbctl delete`. It is not the bridge type — that is always `bridgev2` for this bridge.

To start over from a clean slate, `bbctl delete sh-mattermost` removes the bridge and its rooms on the Beeper side (and the local data directory). It does not touch your Mattermost server.

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

## Presence (optional)

Mattermost knows who is around, and the bridge can hand that to Matrix as **presence on
the ghost of each user**. Set `network.presence.enabled: true`:

| Mattermost | Matrix                                   |
| ---------- | ---------------------------------------- |
| `online`   | `online`                                 |
| `away`     | `unavailable`                            |
| `dnd`      | `online`, status message "Do Not Disturb" |
| `offline`  | `offline`                                |

Off by default: every poll costs Mattermost requests, and a homeserver that does not
store presence gains nothing but the traffic.

Where the status comes from: `POST /api/v4/users/status/ids`, read every 30 seconds for
the users the bridge knows. It is not pushed, because Mattermost addresses its
websocket `status_change` event at the affected user's own connections — a bridge
connected as one user only ever hears about that user.

What it covers, and what it deliberately leaves alone:

- Only users the bridge has already bridged. A status for someone with no ghost (they
  never posted in a bridged channel) is skipped, not turned into a new ghost.
- The logged-in user's own status. Their MXID is a real Matrix account, and presence is
  set by their own clients; the bridge must not fight them.
- A user who disappears from the status answer. Silence is not the same as going
  offline.
- The homeserver has to support presence. Synapse stores it and shares it with users in
  the same rooms. Beeper's hungryserv serves a narrow Matrix subset and drops it, so a
  Beeper-hosted bridge shows no presence and logs no error.

## Names and mentions

`network.display_names` sets how Mattermost people are named in Matrix: their names and
the text of `@mention` pills. Mentions themselves always go out and are matched by
username, whatever is displayed.

| Value | Name shown |
|---|---|
| `server` (default) | As Mattermost shows them to you: your own *Display → Teammate Name Display* preference, else the server's setting (unless the server locks it) |
| `username` | `alice` |
| `full_name` | `Alice Example`; falls back to the username |
| `nickname` | Nickname, then full name, then username |

The setting is read at connect, and every member's name is refreshed on the startup
sync, so a change applies to existing names at the next restart. An unknown value
stops the bridge at startup with an error naming the option.

## Status on DM titles

`network.dm_status` shows the other person's Mattermost status on a 1:1 DM's title,
which is visible in the chat list and header. Their custom status emoji follows it, and
its text becomes the DM's topic. Title changes do not appear in the timeline. Channels and
group DMs keep their names: one shared title cannot show each member's status.

| Value | Title |
|---|---|
| `all` (default) | `alice 🟢` online, `alice 🟡` away, `alice ⛔` do not disturb, `alice ⚪` offline |
| `dnd` | `alice ⛔` while do not disturb is on, otherwise `alice` |
| `off` | `alice` |

With a custom status: `alice 🟢 🌴`, topic "On vacation". Server custom emoji (uploaded
images) cannot appear in a text title and are left out; expired custom statuses are
cleared.

Statuses come from the same 30-second poll as presence (see below), which runs whenever
either feature is on. Matrix presence itself is not needed, which matters on Beeper:
its homeserver drops presence, but titles always show.

## One login per team

Each Mattermost team is a separate login, so clients that group and filter by account
(such as Beeper's inbox toggles) treat each team as its own account. The login is named
after the team and uses the team icon. Log in once per team with the same credentials;
when you belong to more than one team, the login asks which team to bridge.

When your account is in more than one team, team channels are named with their team,
such as `Town Square · Example`, since a client may show every login with the same icon.

DMs and group DMs belong to no team, so exactly one of your logins carries them: your
first login, unless you choose another with `dm-team <team>` in the bridge's management
room (`dm-team` alone lists your logins). Moving them recreates the DM rooms under the
new login, with history re-imported.

A login made before logins had teams is pinned on its next connect to your first team
by name, and keeps the DMs. Its rooms for other teams' channels are removed; add a login
for each other team to bridge them again.

`network.team_spaces: true` additionally puts a login's channels in a space for the team.
It is off by default: Beeper's inbox filters do not apply to rooms inside these spaces.

## Read sync

Reading is synced both ways. Reading a room or thread in Matrix marks it read in
Mattermost, and viewing a channel in Mattermost marks it read in Matrix. Mattermost can
only mark a channel read up to the present, so a message arriving in the second between
your client showing a room and the bridge passing on the receipt is marked read as well.

## Slash commands

A message from Matrix that starts with a slash command you can run in Mattermost,
such as `/away`, `/dnd`, `/header New topic` or a plugin's `/meet start`, runs
the command in that channel (or thread) instead of being posted, as typing it in
Mattermost does. The bridge asks Mattermost which commands exist in the team, so
plugin commands work too.

- Replies only you can see (for example "You are now away") appear as notices
  from the bridge. Their formatting and links carry over; buttons only work in
  Mattermost.
- Anything else starting with `/` is posted as text, so `/usr/bin is full` is
  not swallowed. To post text that starts with a command, begin it with `//`:
  `//away` posts "/away".
- Your command stays in the Matrix room as your record. It has no Mattermost
  post, so it cannot be edited or reacted to; deleting it only removes it from
  Matrix.
- If Mattermost refuses a command, its error is shown on the failed message.

Some Matrix clients handle a few commands themselves (`/me`, `/shrug`); those
never reach the bridge.

## About this fork

This fork tracks upstream and carries the changes below. They are also open as upstream pull requests, so some or all of them may eventually be merged there.

| Change | Upstream PR |
|---|---|
| Native Matrix threading for Mattermost threads | [#8](https://github.com/bostrot/mautrix-mattermost/pull/8) |
| Channel lifecycle: an archived channel keeps its room, shows Mattermost's own archive/restore message and becomes read-only; a permanently deleted channel removes its room, including one deleted while the bridge was offline | [#9](https://github.com/bostrot/mautrix-mattermost/pull/9) |
| Reactions are backfilled with the messages they belong to | [#10](https://github.com/bostrot/mautrix-mattermost/pull/10) (merged) |
| Linked Mattermost posts are quoted under their permalinks | [#11](https://github.com/bostrot/mautrix-mattermost/pull/11) |
| @mentions in both directions, full room member lists, current ghost names, configurable display names | not yet opened |
| Status on DM titles (`network.dm_status`) | fork-only for now |
| Read sync, one space per team | fork-only for now |

Optional presence publishing (see [Presence](#presence-optional)) is fork-only and has
no upstream PR yet.

How the lifecycle behaves, and what to watch for:

- **Archive** raises the room's `events_default` (and any explicit message overrides) to power level 50 so members cannot post. It never lowers a stricter setting, restores only what it changed, and leaves an administrator's own edits alone.
- **Delete** removes the room only after the server answers with a canonical 404 for an authenticated session. Anything uncertain (a 403, a server error, a dropped connection) keeps the room.
- Matrix has no compare-and-set for room state, so an administrator edit made at the exact moment of an archive or restore can be overwritten.
- The deferred delete check requires `async_events: false` in the bridge config. `bbctl` generates it that way; if you set it to `true` the bridge keeps rooms instead of deleting them.
- The bridge polls the channel list about every 30 seconds to catch events the Mattermost websocket missed.

Releases are tagged with plain semver, starting at `v0.1.0`, and the version the binary reports matches the tag. The `main` branch mirrors upstream; this branch (`fpl-main`) is the one to use.

A fork release does not need to match an upstream version number. Pass `-fpl` as build metadata or a suffix only if you later want to distinguish a fork build from an upstream one.

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

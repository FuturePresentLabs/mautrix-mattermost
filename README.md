# mautrix-mattermost
[![License](https://img.shields.io/github/license/bostrot/mautrix-mattermost.svg)](LICENSE)

A Matrix–[Mattermost](https://mattermost.com) bridge built on the [mautrix bridgev2](https://github.com/mautrix/go) framework.

> **This is a fork** of [bostrot/mautrix-mattermost](https://github.com/bostrot/mautrix-mattermost), with additional bridge features and ready-to-run releases. See [About this fork](#about-this-fork).

Bridges text, media, reactions, edits, deletions, replies, emoji shortcodes, typing notifications, and backfill. Supports Personal Access Token (PAT) and email+password login. All teams, channels, and DMs are bridged automatically on login.

## Requirements

- Go 1.26+ and a C compiler (only for building from source; SQLite needs CGO)
- A Matrix homeserver
- SQLite (default) or PostgreSQL
- A Mattermost server with API v4

## Installation

### Download a release (easiest)

Binaries are attached to each [release](https://github.com/jameswagar/mautrix-mattermost/releases) with a checksum, for these platforms:

| Platform | File |
|---|---|
| Linux x86_64 | `mautrix-mattermost-linux-amd64.tar.gz` |
| Linux ARM64 | `mautrix-mattermost-linux-arm64.tar.gz` |
| macOS Apple Silicon | `mautrix-mattermost-darwin-arm64.tar.gz` |
| macOS Intel | `mautrix-mattermost-darwin-amd64.tar.gz` |

```sh
# pick the file for your platform
file=mautrix-mattermost-linux-amd64.tar.gz
curl -LO "https://github.com/jameswagar/mautrix-mattermost/releases/latest/download/$file"
curl -LO "https://github.com/jameswagar/mautrix-mattermost/releases/latest/download/$file.sha256"

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
git clone https://github.com/jameswagar/mautrix-mattermost.git
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

## Teams

By default one login covers your whole Mattermost account, the way Beeper's Discord
bridge covers a Discord account:

- Your DMs and group DMs are always bridged.
- Each team is switched off until you switch it on. In the bridge's management room,
  `team` lists your teams with their state, and `team on <team>` or `team off <team>`
  switches one (by name, handle or ID). Switching a team off removes its rooms.
- Each switched-on team gets a space named and pictured like the team, holding its
  channels (`network.team_spaces`, on by default).
- New channel rooms start muted, so channels stay quiet while DMs notify; unmute the
  ones you want to hear from (`network.mute_channels_on_create`, on by default). Rooms
  that already exist are never re-muted.

The account's own space, which holds the DMs and the team spaces, is named
`Mattermost DMs` (`network.account_space_name`; empty keeps the framework's
`Mattermost (<username>)`).

When your account is in more than one team and team spaces are off, team channels are
named with their team, such as `Town Square · Example`. With team spaces on the names
stay plain, since Beeper already shows a chat in a space as `<chat> · <space>`.

### One login per team

With `network.team_logins: true`, each team is a separate login instead: log in once
per team, and the login asks which team to bridge. The login is named after the team
and uses its icon. Exactly one of the logins carries the DMs: your first, unless you
choose another with `dm-team <team>` (`dm-team` alone lists them). A login made before
logins had teams is pinned to your first team by name on its next connect.

## Thread reads

Matrix keeps a read marker per thread, so the bridge marks threads read as well as rooms:

- Reading a thread in Mattermost marks it read in Matrix (and the other way round).
- Viewing a channel in Mattermost marks its threads read, except threads Mattermost counts
  as unread (with collapsed reply threads on, only followed threads can be unread there).
- On connect, rooms and threads Mattermost already has as read are marked read in Matrix.

This clears thread unread counts (such as Beeper's Unread threads) for threads you read in
Mattermost.

## Replies

Mattermost replies are threads. With the [Channel Reply](https://github.com/Azario16/mattermost-plugin-channel-reply)
plugin on the server, Mattermost also has replies to one message, posted in the channel with
a quote of it, and the bridge uses them (`network.quoted_replies`, on by default):

- A reply sent from Matrix outside a thread becomes a quoted reply in the channel. Inside a
  thread it becomes a quoted reply in that thread. Edits keep the quote.
- A quoted reply made in Mattermost arrives as a Matrix reply to the message it quotes,
  without the quote block.

The bridge asks the server whether the plugin is active (every 10 minutes). Without it, or
with the option off, a reply goes to the thread of the message replied to.

## Reactions that act

In a hurry nobody should be hunting for a particular emoji, so for two kinds of
post **any reaction** is the gesture, and it is not sent to Mattermost as an emoji:

- **Burn-on-read message from someone else.** It arrives as a placeholder that
  says any reaction reveals it. The text then disappears when the countdown the
  server reports ends. The countdown and the longest a message may exist both come
  from the server's burn-on-read settings; the bridge holds no copy of either.
- **Post that asks for an acknowledgement** (from someone else). Beeper has no
  acknowledge button, so the message ends with a line saying so: "✅ Any reaction
  acknowledges." Any reaction acknowledges it in Mattermost. A second reaction
  does not acknowledge again.
  The acknowledgement then appears on the message as a `✅ by <name>` reaction from
  the bridge, like everyone else's.

Revealing and acknowledging are separate acts. On a message that is both, the
first reaction reveals it and a later reaction acknowledges it. The
placeholder then says "After revealing it, react again to acknowledge."

The line disappears once this bridged account acknowledges the post, whether you
acknowledge from Beeper or in Mattermost, and comes back if that acknowledgement
is withdrawn. Someone else acknowledging does not hide it. This applies to plain
text, unrevealed burn placeholders, and text revealed through this bridge while
both the viewer's expiry and the original Matrix removal deadline remain valid.
The revealed-text edit uses the already-fetched server snapshot: it never
reveals again, retains another plaintext copy, or restarts the countdown.
Messages with attachments, quoted replies or link previews keep the line until
the post itself is edited, since rebuilding those bodies could drop content.
Messages bridged before prompt tracking existed are left unchanged.

Once a reaction has revealed or acknowledged, the bridge redacts it, so it does
not look as though that emoji was sent. If the reveal or acknowledgement fails
the reaction stays, with the failure shown against it.

Everything else, including your own posts and any post the bridge cannot read, is
an ordinary emoji reaction. Removing one of these reactions does nothing in
Mattermost, because nothing was added there. Withdrawing an acknowledgement is done
in Mattermost, which also decides how long that stays possible; the bridge holds no
time value for it.

## Read sync

Reading is synced both ways. Reading a room or thread in Matrix marks it read in
Mattermost, and viewing a channel in Mattermost marks it read in Matrix. Mattermost can
only mark a channel read up to the present, so a message arriving in the second between
your client showing a room and the bridge passing on the receipt is marked read as well.

## Link previews and server messages

Link previews Mattermost made for a message (title, description, site and
image) are sent with it as bundled previews (`com.beeper.linkpreviews`), so
clients that support them show the preview without fetching the page. The
bridge downloads the image (http or https, up to 5 MB) and uploads it; if that
fails, the preview is sent without an image.

Mattermost's own messages, such as "alice joined the channel" or "bob added to
the channel by alice", come from the bridge bot as notices, not from the person
they name.

## Loading older history

Clients such as Beeper load older messages on demand when you scroll back. The
bridge supports this, but the framework keeps it off by default; turn it on in
the bridge config:

```yaml
backfill:
    queue:
        manual: true
```

## Mutes

Channels you mute in Mattermost ("Mute channel") are muted in Matrix too:

- A room created for a muted channel starts muted.
- Rooms that existed before this was added are muted once where their channel
  is muted; nothing is unmuted by this.
- Muting or unmuting a channel in Mattermost later does the same to its room.

It only goes one way. Muting a room in Matrix stays in Matrix and does not
change your Mattermost notifications.

## Slash commands

A message from Matrix that starts with a slash command you can run in Mattermost,
such as `/away`, `/dnd`, `/header New topic` or a plugin's `/meet start`, runs
the command in that channel (or thread) instead of being posted, as typing it in
Mattermost does. The bridge asks Mattermost which commands exist in the team, so
plugin commands work too.

- Replies only you can see (for example "You are now away") appear as notices
  from the bridge. Their formatting and links carry over; buttons only work in
  Mattermost. Mattermost does not tell the bridge when you dismiss one, so they
  are removed after 24 hours by default. Set `network.expire_ephemeral_notices`
  to `false` to keep new private replies. Bridge status messages, ordinary bot
  messages, and your own commands are not removed.
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

This fork tracks upstream, ships its own releases, and contributes changes back
where practical. Several features below have already merged upstream; the
remaining lifecycle follow-ups are separate PRs.

| Change | Upstream status |
|---|---|
| Native Matrix threading for Mattermost threads | [#8](https://github.com/bostrot/mautrix-mattermost/pull/8) — merged |
| Authenticated channel-deletion probe and channel restoration handling | [#9](https://github.com/bostrot/mautrix-mattermost/pull/9) — open, awaiting re-review |
| Reactions backfilled with their messages | [#10](https://github.com/bostrot/mautrix-mattermost/pull/10) — merged |
| Linked Mattermost posts quoted under their permalinks | [#11](https://github.com/bostrot/mautrix-mattermost/pull/11) — merged |
| Transient websocket-disconnect status | [#12](https://github.com/bostrot/mautrix-mattermost/pull/12) — merged |
| File names kept separate from captions | [#13](https://github.com/bostrot/mautrix-mattermost/pull/13) — merged |
| Mentions in both directions, full member lists and display-name options | [#14](https://github.com/bostrot/mautrix-mattermost/pull/14) — merged |
| Two-way read-state sync | [#15](https://github.com/bostrot/mautrix-mattermost/pull/15) — merged |
| Integration-message attachments | [#16](https://github.com/bostrot/mautrix-mattermost/pull/16) — merged |
| One Matrix space per Mattermost team | [#17](https://github.com/bostrot/mautrix-mattermost/pull/17) — merged |
| Read-only archived rooms with reversible power levels | [#21](https://github.com/bostrot/mautrix-mattermost/pull/21) — draft; rebase after #9 merges |
| Opt-in lifecycle polling with a configurable interval | [#22](https://github.com/bostrot/mautrix-mattermost/pull/22) — draft; rebase after #9 merges |
| DM-title status, optional presence publishing, burn-on-read and acknowledgement workflows | Fork-only; no upstream PRs yet |

The fork's existing lifecycle implementation is broader than the small core fix
in #9. The separate drafts do not change what is already available in fork
releases.

How the lifecycle behaves, and what to watch for:

- **Archive** raises the room's `events_default` (and any explicit message overrides) to power level 50 so members cannot post. It never lowers a stricter setting, restores only what it changed, and leaves an administrator's own edits alone.
- **Delete** removes the room only after the server answers with a canonical 404 for an authenticated session. Anything uncertain (a 403, a server error, a dropped connection) keeps the room.
- Matrix has no compare-and-set for room state, so an administrator edit made at the exact moment of an archive or restore can be overwritten.
- The deferred delete check requires `async_events: false` in the bridge config. `bbctl` generates it that way; if you set it to `true` the bridge keeps rooms instead of deleting them.
- The bridge polls the channel list about every 30 seconds to catch events the Mattermost websocket missed.

Releases use plain semantic version tags, and the binary's reported version
matches the tag. Use the default `jw-main` branch for this fork. Fork release
numbers are independent of upstream's version numbers.

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

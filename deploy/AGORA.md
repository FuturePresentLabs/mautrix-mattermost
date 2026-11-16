# Running the bridge on agora-one

This is the recipe used to stand the bridge up on the `agora-one` host. It assumes a
Linux x86_64 machine with a C compiler (SQLite needs CGO) and Go available:

```sh
mkdir -p ~/.local && curl -sL https://go.dev/dl/go1.27.4.linux-amd64.tar.gz | tar -C ~/.local -xz
```

## Build and install

```sh
mkdir -p ~/mautrix-mattermost/data
rsync -az --exclude .git --exclude /mautrix-mattermost ./ agora-one:src/mautrix-mattermost/
ssh agora-one 'cd ~/src/mautrix-mattermost && \
  PATH=$HOME/.local/go/bin:$PATH go build -tags goolm -ldflags "-s -w" \
  -o ~/mautrix-mattermost/mautrix-mattermost ./cmd/mautrix-mattermost'
```

`-tags goolm` avoids the libolm C dependency; SQLite still needs `gcc`.

The binary lives at `~/mautrix-mattermost/mautrix-mattermost`, next to `config.yaml`
and the `data/` directory that holds the SQLite database.

## Configuration

Which homeserver the bridge registers with decides where it runs.

**Beeper.** Beeper provisions the appservice and hands back a config whose
`homeserver.address` is `https://matrix.beeper.com/_hungryserv/<user>` and whose
`homeserver.domain` is `beeper.local`, with appservice websocket mode instead of an
inbound URL. `bbctl` does the provisioning:

```sh
bbctl login                                            # interactive, once
bbctl config --type bridgev2 sh-mymattermost -o config.yaml
rsync config.yaml agora-one:mautrix-mattermost/
```

**A homeserver you run yourself.** Generate the config and registration from the
binary, then add the registration to the homeserver:

```sh
cd ~/mautrix-mattermost
./mautrix-mattermost -e -c config.yaml       # template; edit homeserver + database
./mautrix-mattermost -g -c config.yaml -r registration.yaml
# add registration.yaml to the homeserver's app_service_config_files and restart it
```

Either way, set `database.type: sqlite3-fk-wal` and
`database.uri: file:data/mautrix-mattermost.db?_txlock=immediate` for a
single-machine install, and fill in `bridge.permissions`. An empty map — and a map
that holds only the `"*"` wildcard — counts as unconfigured and is a startup error:

```yaml
bridge:
    permissions:
        "*":
            send_events: true
            commands: true
            login: true
        "example.org":            # your homeserver domain, spelled out
            send_events: true
            commands: true
            login: true
        "@admin:example.org":
            admin: true
            send_events: true
            commands: true
            login: true
```

## Smoke test

The binary can be started before a homeserver exists to check the install: it creates
the SQLite schema, binds the appservice port and retries the homeserver connection.

```sh
cd ~/mautrix-mattermost
./mautrix-mattermost -e -c config.smoke.yaml   # template
./mautrix-mattermost -g -c config.smoke.yaml -r registration.smoke.yaml
./mautrix-mattermost -c config.smoke.yaml      # expect: "Starting HTTP listener" + retries
```

## Service

`deploy/mautrix-mattermost.service` is a systemd **user** unit:

```sh
mkdir -p ~/.config/systemd/user
cp deploy/mautrix-mattermost.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now mautrix-mattermost
loginctl enable-linger "$USER"    # keep it running after logout
journalctl --user -u mautrix-mattermost -f
```

## Presence

`network.presence.enabled: true` republishes Mattermost statuses as Matrix presence —
see [Presence](../README.md#presence-optional). It is off by default and only reaches
clients when the homeserver stores presence; Beeper's hungryserv does not, so on a
Beeper-hosted bridge the flag costs requests and changes nothing.

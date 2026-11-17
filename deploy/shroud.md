# Running mautrix-mattermost in a Shroud microVM

Shroud boots a **prepared rootfs** (SquashFS + initramfs), not a container. The
image is built once from this repository's `Dockerfile`, converted with
`shroudoci`, and pinned by digest. Inside the guest:

* the entrypoint runs as root; there is no init, systemd, or Docker;
* the root filesystem is an ephemeral overlay over the immutable image;
* `/tmp` and `/var/tmp` are tmpfs and disappear on restart;
* only a declared block volume under `/data` survives a VM restart or image roll;
* deployment `env` and secret references are sourced before the entrypoint runs.

The Dockerfile is already microVM-shaped: config, registration, and the SQLite
database live under `/data/mautrix-mattermost`. On older Shroud nodes that only
expose a virtiofs volume at `/var/lib/registry`, the entrypoint detects that
mount and uses `/var/lib/registry/mautrix-mattermost` instead.

## 1. Build and publish the rootfs

Build the OCI image on a Linux builder with `buildah` (or Docker), then convert
it. `shroudoci extract` reads the packaged `ENTRYPOINT`, so no `reinit` override
is needed — keep the image entrypoint as-is.

```sh
# On a build host with buildah + shroudoci:
buildah bud --format docker -t localhost/fpl/mautrix-mattermost:fab -f Dockerfile .

out=/var/lib/shroud/images/mautrix-mattermost-build
sudo shroudoci extract localhost/fpl/mautrix-mattermost:fab "$out" \
  --hub containers-storage:

# Publish to the region artifact store. The printed rootfs:<name>@sha256:...
# reference is what Terraform/Shroud deploys.
sudo sh -c 'set -a; . /etc/shroud/artifacts.env; set +a
  shroudoci push "$0" --name fpl/mautrix-mattermost' "$out"
```

With Docker instead of buildah, use `docker build` and
`--hub docker-daemon:`.

Do **not** replace the entrypoint with `shroudoci reinit --process-arg ...`.
Shroud runs the artifact's packaged argv; a `process.args` value in a deployment
request does not replace it. The packaged argv is
`/usr/local/bin/mautrix-mattermost-entrypoint`.

## 2. Prepare the persistent volume

The bridge needs one block volume mounted at `/data/mautrix-mattermost`. Create
it on the Shroud host, then populate it while the VM is stopped:

```sh
sudo truncate -s 1G /var/lib/shroud/persistence/mautrix-mattermost.ext4
sudo mkfs.ext4 -q /var/lib/shroud/persistence/mautrix-mattermost.ext4
sudo mkdir -p /mnt/mm && sudo mount -o loop \
  /var/lib/shroud/persistence/mautrix-mattermost.ext4 /mnt/mm

sudo install -d -m 0700 /mnt/mm/data
sudo install -m 0600 config.yaml /mnt/mm/config.yaml
# standalone only; omit for Beeper. Generate it first with:
#   ./mautrix-mattermost -g -c config.yaml -r registration.yaml
if [ -f registration.yaml ]; then
  sudo install -m 0600 registration.yaml /mnt/mm/registration.yaml
fi

sudo umount /mnt/mm
```

Layout on the volume:

```text
config.yaml
registration.yaml        # standalone only; not used with Beeper
data/                    # SQLite database, WAL files, logs if file logging is on
```

If you prefer not to seed the volume by hand, set
`MAUTRIX_MATTERMOST_CONFIG_B64` (base64 of the YAML) as a Shroud secret
reference; the entrypoint writes `config.yaml` once, on first boot. Shroud
limits each env value to 16 KiB, so strip comments from the config first
(`./mautrix-mattermost -e` output is ~27 KiB commented and ~6 KiB stripped).

## 3. Config that matters in a microVM

Start from `./mautrix-mattermost -e -c config.yaml` and set at least:

```yaml
database:
  type: sqlite3-fk-wal
  uri: file:/data/mautrix-mattermost/data/mautrix-mattermost.db?_txlock=immediate

homeserver:
  address: https://matrix.example.com    # or the Beeper hungryserv URL
  domain: beeper.local                    # or your homeserver domain
  websocket: true                         # Beeper/hungryserv only

appservice:
  address: http://mautrix-mattermost.internal.example:29345
  hostname: 0.0.0.0                       # required: Shroud port-forwarding hits eth0
  port: 29345

bridge:
  permissions:
    "*":
      send_events: true
      commands: true
      login: true
    "example.org":                        # your real homeserver domain
      send_events: true
      commands: true
      login: true
```

Notes:

* `bridge.permissions` must contain an entry besides the example placeholder,
  or startup fails with `bridge.permissions not configured`.
* With Beeper, `bbctl config --type bridgev2 sh-mattermost -o config.yaml`
  supplies `as_token`/`hs_token` and websocket mode; the bridge makes only
  outbound connections and needs no inbound port.
* For a self-hosted homeserver, `appservice.address` must be reachable from the
  homeserver (VPC/Palisade route), and the homeserver must load
  `registration.yaml`.
* `database.uri` must point under `/data`; an overlay path loses all logins and
  portal state on restart.

## 4. Declare the service

The rootfs reference must be digest-pinned. The Shroud deploy request is where
volumes, internal-only ingress, and health are expressed (same shape as
`shared-infra/projects/rakazo-beeper`):

```hcl
locals {
  request = {
    service     = "mautrix-mattermost"
    environment = "production"
    image       = "rootfs:fpl/mautrix-mattermost@sha256:..."
    domain      = "mautrix-mattermost.internal.fpl.dev"
    path        = "/"
    internalOnly = true
    promote      = false
    resources    = { cpu = 1, memoryMiB = 512 }
    port = {
      name     = "http"
      hostPort = 29345
      vmPort   = 29345
      protocol = "tcp"
      scheme   = "http"
    }
    # Omit `process` so Shroud boots the packaged entrypoint from the image.
    env = {
      MAUTRIX_MATTERMOST_DATA_DIR = "/data/mautrix-mattermost"
    }
    volumes = [{
      id   = var.state_volume_id   # enrolled block volume
      path = "/data/mautrix-mattermost"
    }]
    health = {
      enabled = true
      # The bridge has no /healthz. The provisioning API is always mounted and
      # returns 401 without a Matrix token, which proves the listener is up.
      path                = "/_matrix/provision/v3/login/flows"
      expectedStatus      = 401
      startupGraceSeconds = 60
    }
  }
}

resource "fpl_shroud_deployment" "mautrix_mattermost" {
  request_id    = "mautrix-mattermost-${var.revision_id}"
  internal_only = true
  secret_refs   = []                            # or ["mautrix-mattermost-config"]; see §2
  request       = jsonencode(local.request)
}
```

The examples of `fpl_shroud_service` in shared-infra expose only `port
{ name, port }` and no volume block, whereas `fpl_shroud_deployment` carries the
full request (volumes, `internalOnly`, health). Use the deployment resource for
this bridge so `/data` survives replacement.

Raw `MicroVM` equivalent (used by `beeper-bridges`-style roots):

```json
{
  "kind": "MicroVM",
  "metadata": { "name": "mautrix-mattermost" },
  "spec": {
    "image": "rootfs:fpl/mautrix-mattermost@sha256:...",
    "resources": { "cpu": 1, "memoryMiB": 512 },
    "process": { "args": ["/usr/local/bin/mautrix-mattermost-entrypoint"] },
    "networking": { "tapDeviceName": "sh-mm", "ports": [] },
    "lifecycle": { "autostart": true, "deleteOnExit": false },
    "health": { "enabled": false },
    "blockVolumes": [{
      "path": "/data/mautrix-mattermost",
      "hostPath": "/var/lib/shroud/persistence/mautrix-mattermost.ext4",
      "seedPath": null,
      "sizeMiB": 1024,
      "ephemeral": false
    }]
  }
}
```

Sizing: 1 CPU / 256 MiB is enough for a small bridge; 512 MiB leaves room for
backfill and media handling. The database grows with bridged history, so give
the volume room and back it up while the VM is stopped.

## 5. Verify

```sh
# In the guest console/logs:
#   Starting HTTP listener  address=0.0.0.0:29345

# From the VPC, the provisioning route answers 401 without a token:
curl -s -o /dev/null -w '%{http_code}\n' \
  http://<vm-ip>:29345/_matrix/provision/v3/login/flows

# The appservice itself answers 404 at / and treats
# /_matrix/app/v1/transactions as a POST-only endpoint.
```

For Beeper, send `login` to the bridge bot, then log in with a Mattermost PAT.

## Container use

The same image runs under Docker for local testing:

```sh
docker build -t mautrix-mattermost .
mkdir -p deploy-data && cp config.yaml deploy-data/
docker run --rm -p 29345:29345 \
  -v "$PWD/deploy-data:/data/mautrix-mattermost" \
  mautrix-mattermost
```

The host directory starts with only `config.yaml`; the entrypoint creates
`data/` and the SQLite database on first start. Runs as root, matching the
microVM.

## CI and automatic updates

`.github/workflows/docker.yml` builds `linux/amd64` and `linux/arm64` images and
pushes them to Docker Hub (`futurepresentlabs/mautrix-mattermost`):

| Trigger | Tags |
| --- | --- |
| push to `fpl-main` | `edge`, `sha-<short>` |
| tag `v*` | `latest`, `X.Y.Z`, `X.Y`, `sha-<short>` |
| pull request | build only |

Add `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN` (Read & Write) as repository
secrets. Nothing is pushed from a pull request.

### Pull-based updates on the Shroud host

The host can follow a published tag without receiving SSH credentials from
GitHub. `deploy/shroud-update.sh` resolves the tag digest, converts the image to
a prepared rootfs with the host's `shroudoci`, swaps the VM image, and rolls back
if the bridge does not log `Bridge started`.

```sh
# On the Shroud host, as root:
install -m 0755 deploy/shroud-update.sh /usr/local/sbin/mautrix-mattermost-update
install -m 0644 deploy/mautrix-mattermost-update.service /etc/systemd/system/
install -m 0644 deploy/mautrix-mattermost-update.timer /etc/systemd/system/
systemd-analyze verify /etc/systemd/system/mautrix-mattermost-update.service
systemctl daemon-reload
systemctl enable --now mautrix-mattermost-update.timer
```

It follows `latest` by default. Set `MM_TAG=edge` in the unit environment to
track every `fpl-main` build, or override `MM_IMAGE` for a different registry or
namespace. An authenticated Docker Hub pull (`skopeo login docker.io`) avoids
anonymous rate limits.

The state, lock, and conversion log live in `/var/lib/mautrix-mattermost-updater`.
The previous image is kept at `<runtime>/images/<vm>.prev` for manual rollback;
the persistent `/data` volume is never modified by the updater.

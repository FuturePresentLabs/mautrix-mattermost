#!/bin/sh
# Entrypoint for both the container image and the Shroud microVM rootfs.
#
# On Shroud the guest init runs this script as root, sources the deployment's
# env/secrets, and mounts persistent block volumes under /data. Everything
# outside /data is an ephemeral overlay, so all state (config, registration,
# SQLite database) must live under MAUTRIX_MATTERMOST_DATA_DIR.
set -eu

bin=${MAUTRIX_MATTERMOST_BINARY:-/usr/local/bin/mautrix-mattermost}
# Newer Shroud mounts block volumes at the declared path under /data. Older
# hosts expose a virtiofs volume at /var/lib/registry and ignore blockVolumes,
# so fall back to that mount instead of losing state in the overlay.
if [ -n "${MAUTRIX_MATTERMOST_DATA_DIR:-}" ]; then
    data_dir=$MAUTRIX_MATTERMOST_DATA_DIR
elif grep -qs ' /var/lib/registry ' /proc/mounts; then
    data_dir=/var/lib/registry/mautrix-mattermost
else
    data_dir=/data/mautrix-mattermost
fi
config_path=${MAUTRIX_MATTERMOST_CONFIG:-$data_dir/config.yaml}
registration_path=${MAUTRIX_MATTERMOST_REGISTRATION:-$data_dir/registration.yaml}

log() {
    printf '%s mautrix-mattermost-entrypoint: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2
}

if [ ! -d "$data_dir" ]; then
    mkdir -p "$data_dir"
fi

if [ ! -x "$bin" ]; then
    log "error: bridge binary not found or not executable: $bin"
    exit 64
fi

# Materialize config.yaml on first boot. The preferred path is to seed the
# persistent volume with config.yaml while the volume is unmounted. For
# deployments where config comes from a Shroud secret reference, accept the
# whole YAML document in MAUTRIX_MATTERMOST_CONFIG (or base64 in
# MAUTRIX_MATTERMOST_CONFIG_B64) and write it once. Write to a sibling and
# rename so a failed decode cannot leave a truncated config behind.
if [ ! -f "$config_path" ]; then
    config_dir=$(dirname "$config_path")
    mkdir -p "$config_dir"
    tmp_config="$config_path.tmp.$$"
    trap 'rm -f "$tmp_config"' EXIT HUP INT TERM

    if [ -n "${MAUTRIX_MATTERMOST_CONFIG_B64:-}" ]; then
        log "writing config from MAUTRIX_MATTERMOST_CONFIG_B64"
        printf '%s' "$MAUTRIX_MATTERMOST_CONFIG_B64" | base64 -d >"$tmp_config"
    elif [ -n "${MAUTRIX_MATTERMOST_CONFIG:-}" ]; then
        log "writing config from MAUTRIX_MATTERMOST_CONFIG"
        printf '%s\n' "$MAUTRIX_MATTERMOST_CONFIG" >"$tmp_config"
    else
        log "error: $config_path does not exist"
        log "seed the /data volume with config.yaml, or set MAUTRIX_MATTERMOST_CONFIG_B64"
        log "as a Shroud secret and retry"
        exit 64
    fi

    chmod 600 "$tmp_config"
    mv "$tmp_config" "$config_path"
    trap - EXIT HUP INT TERM
fi

# One-time standalone registration. Beeper/hungryserv deployments get their
# registration and as_token/hs_token from bbctl and must not run this.
if [ "${MAUTRIX_MATTERMOST_GENERATE_REGISTRATION:-0}" = "1" ] && [ ! -f "$registration_path" ]; then
    log "generating appservice registration at $registration_path"
    "$bin" -g -c "$config_path" -r "$registration_path"
fi

if [ "$#" -gt 0 ]; then
    exec "$bin" "$@"
fi

exec "$bin" -c "$config_path"

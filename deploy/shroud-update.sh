#!/bin/sh
# Pull-based updater for the mautrix-mattermost Shroud microVM.
#
# Runs on the Shroud host, resolves the published Docker Hub image, converts it
# to a prepared rootfs with the host's shroudoci, and swaps the VM image with a
# rollback on failure. The persistent block volume is never touched.
#
# Intended to run from a systemd timer (see the .service/.timer examples next to
# this file). Requires root: it talks to the Shroud control socket and shroudoci
# creates device nodes.
#
# Environment:
#   MM_IMAGE   Docker Hub repository   (default futurepresentlabs/mautrix-mattermost)
#   MM_TAG     Tag to follow           (default latest; use edge for fpl-main)
#   MM_VM      Shroud VM name          (default mautrix-mattermost)
#   MM_STATE   State directory         (default /var/lib/mautrix-mattermost-updater)
#   SHROUD_RUNTIME_DIR                 (default /var/lib/shroud)
set -eu
umask 077

IMAGE=${MM_IMAGE:-futurepresentlabs/mautrix-mattermost}
TAG=${MM_TAG:-latest}
VM=${MM_VM:-mautrix-mattermost}
STATE_DIR=${MM_STATE:-/var/lib/mautrix-mattermost-updater}
RUNTIME=${SHROUD_RUNTIME_DIR:-/var/lib/shroud}
SOCKET=$RUNTIME/control.sock
IMAGES=$RUNTIME/images
STATE=$STATE_DIR/image.digest
LOG=$STATE_DIR/update.log

log() {
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

die() {
    log "error: $*"
    exit 1
}

control() {
    printf '%s\n' "$1" | socat - "UNIX-CONNECT:$SOCKET"
}

vm_power() {
    control '{"action":"list"}' |
        jq -r --arg vm "$VM" '.vms[]? | select(.name == $vm) | .power'
}

vm_logs() {
    control "{\"action\":\"logs\",\"vm\":\"$VM\",\"tail\":200}" |
        jq -r '.logs.lines[]?'
}

for tool in skopeo shroudoci socat jq flock; do
    command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done

install -d -m 0700 "$STATE_DIR"
exec 9>"$STATE_DIR/lock"
flock -n 9 || { log "another update is already running"; exit 0; }

# Authenticate Docker Hub pulls if a credential file was provisioned by the
# operator (skopeo reads the same containers auth file as podman/docker).
# `skopeo login docker.io` writes it; anonymous pulls are still supported.

log "checking $IMAGE:$TAG"
remote_digest=$(skopeo inspect --format '{{.Digest}}' "docker://$IMAGE:$TAG") ||
    die "could not resolve $IMAGE:$TAG"

if [ -f "$STATE" ] && [ "$(cat "$STATE")" = "$remote_digest" ]; then
    log "already at $remote_digest"
    exit 0
fi

next=$IMAGES/$VM.next
prev=$IMAGES/$VM.prev
rm -rf "$next"
install -d -m 0755 "$next"

log "converting $IMAGE:$TAG ($remote_digest)"
if ! shroudoci extract "$IMAGE:$TAG" "$next" --hub docker:// >"$LOG" 2>&1; then
    rm -rf "$next"
    die "shroudoci extract failed; see $LOG"
fi
[ -s "$next/rootfs.sqsh" ] || die "converted image has no rootfs.sqsh"
[ -s "$next/initramfs.cpio" ] || die "converted image has no initramfs.cpio"

if [ "$(vm_power)" = "running" ]; then
    log "stopping $VM"
    control "{\"action\":\"stop\",\"vm\":\"$VM\"}" >/dev/null
    i=0
    while [ "$(vm_power)" = "running" ] && [ "$i" -lt 30 ]; do
        i=$((i + 1))
        sleep 1
    done
    [ "$(vm_power)" = "running" ] && die "$VM did not stop"
fi

log "swapping image"
rm -rf "$prev"
if [ -d "$IMAGES/$VM" ]; then
    mv "$IMAGES/$VM" "$prev"
fi
mv "$next" "$IMAGES/$VM"

log "starting $VM"
control "{\"action\":\"start\",\"vm\":\"$VM\"}" >/dev/null

i=0
while [ "$i" -lt 90 ]; do
    i=$((i + 1))
    [ "$(vm_power)" = "running" ] && break
    sleep 1
done

ok=0
if [ "$(vm_power)" = "running" ] && vm_logs | grep -q "Bridge started"; then
    ok=1
fi

if [ "$ok" != 1 ]; then
    log "new image did not become healthy; rolling back"
    control "{\"action\":\"stop\",\"vm\":\"$VM\"}" >/dev/null || true
    rm -rf "$IMAGES/$VM.failed"
    mv "$IMAGES/$VM" "$IMAGES/$VM.failed"
    if [ -d "$prev" ]; then
        mv "$prev" "$IMAGES/$VM"
        control "{\"action\":\"start\",\"vm\":\"$VM\"}" >/dev/null || true
    fi
    die "rolled back to the previous image"
fi

printf '%s\n' "$remote_digest" >"$STATE"
log "updated $VM to $remote_digest"

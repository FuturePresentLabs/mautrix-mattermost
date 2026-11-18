#!/bin/sh
MAUTRIX_VERSION=$(cat go.mod | grep 'maunium.net/go/mautrix ' | awk '{ print $2 }' | head -n1)
# On an exactly tagged commit, report that tag as the version (see main.go).
# Otherwise use the latest tag reachable from HEAD as the base, so a dev build
# reads "v0.1.7+dev.<commit>" rather than the base version in main.go.
TAG=$(git describe --exact-match --tags 2>/dev/null)
BASE=$TAG
[ -n "$BASE" ] || BASE=$(git describe --tags --abbrev=0 2>/dev/null)
VERSION_FLAG=""
[ -n "$BASE" ] && VERSION_FLAG="-X main.Version=${BASE#v}"
GO_LDFLAGS="-s -w -X main.Tag=$TAG $VERSION_FLAG -X main.Commit=$(git rev-parse HEAD) -X 'main.BuildTime=`date -Iseconds`' -X 'maunium.net/go/mautrix.GoModVersion=$MAUTRIX_VERSION'"
go build -tags goolm -ldflags="$GO_LDFLAGS" ./cmd/mautrix-mattermost "$@"

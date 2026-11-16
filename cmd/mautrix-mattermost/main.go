package main

import (
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/bostrot/mautrix-mattermost/pkg/connector"
)

// Build metadata injected at link time via -X linker flags.
//
// Version is the base version; build.sh overrides it with the release tag (minus
// the leading "v") so a tagged build reports that tag. The version helper only
// prints a plain release version when the tag equals this base version, and
// otherwise labels the build a dev build.
var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
	Version   = "0.1.0"
)

func main() {
	m := mxmain.BridgeMain{
		Name:        "mautrix-mattermost",
		Description: "A Matrix-Mattermost bridge",
		URL:         "https://github.com/bostrot/mautrix-mattermost",
		Version:     Version,
		Connector:   &connector.MattermostConnector{},
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}

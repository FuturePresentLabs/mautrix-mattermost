package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Membership events for another user reach every login of the account. A login
// that does not bridge the channel must stay silent: the framework creates a
// portal for any event it is handed, which is how a deleted channel ended up
// with an empty portal under the login that never bridged it.
func TestChannelMemberEventIgnoresChannelsThisLoginDoesNotBridge(t *testing.T) {
	c := &MattermostClient{
		UserID: "me",
		UserLogin: &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "login"},
		},
	}
	c.UserLogin.Metadata = &UserLoginMetadata{TeamID: "team-a"}

	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(evt bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, evt)
		return bridgev2.EventHandlingResultSuccess
	}

	// Not bridged here at all.
	c.handleChannelMemberEvent(&mattermost.ChannelMemberEvent{
		ChannelID: "unknown", UserID: "someone", Added: false})
	if len(queued) != 0 {
		t.Fatalf("queued %d events for a channel this login does not bridge", len(queued))
	}

	// Bridged by another team's login.
	c.channels = map[string]*mattermost.Channel{
		"other-team": {ID: "other-team", TeamID: "team-b"},
	}
	c.handleChannelMemberEvent(&mattermost.ChannelMemberEvent{
		ChannelID: "other-team", UserID: "someone", Added: false})
	if len(queued) != 0 {
		t.Fatalf("queued %d events for another team's channel", len(queued))
	}

	// Bridged here: the event must go through.
	c.channels["mine"] = &mattermost.Channel{ID: "mine", TeamID: "team-a"}
	c.handleChannelMemberEvent(&mattermost.ChannelMemberEvent{
		ChannelID: "mine", UserID: "someone", Added: false})
	if len(queued) != 1 {
		t.Fatalf("queued %d events for an owned channel, want 1", len(queued))
	}
}

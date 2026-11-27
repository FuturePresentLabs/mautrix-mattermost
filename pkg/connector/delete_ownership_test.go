package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// A channel event reaches every login of the account. A login that never
// bridged the channel has no room to delete, and acting on the hint would
// create a portal for a chat it never had - which is how a deleted channel
// ended up with an empty portal under the other login.
func TestDeleteLifecycleChannelSkipsChannelsThisLoginNeverBridged(t *testing.T) {
	c := &MattermostClient{UserID: "me", UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}}
	queued := 0
	queue := func(bridgev2.RemoteEvent) { queued++ }

	c.hasPortalFn = func(string) bool { return false }
	c.deleteLifecycleChannel("c1", queue)
	if queued != 0 {
		t.Fatalf("queued %d deletes for a channel this login never bridged", queued)
	}

	c.hasPortalFn = func(string) bool { return true }
	c.deleteLifecycleChannel("c1", queue)
	if queued != 1 {
		t.Fatalf("queued %d deletes for an owned channel, want 1", queued)
	}
}

// The hint carries the channel id, so the check must ask about that channel.
func TestDeleteLifecycleChannelChecksTheHintedChannel(t *testing.T) {
	c := &MattermostClient{UserID: "me", UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}}
	var asked []string
	c.hasPortalFn = func(id string) bool { asked = append(asked, id); return false }
	c.deleteLifecycleChannel("channel-x", func(bridgev2.RemoteEvent) {})

	if len(asked) != 1 || asked[0] != "channel-x" {
		t.Fatalf("asked about %v, want [channel-x]", asked)
	}
}

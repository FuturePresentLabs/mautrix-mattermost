package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// Asking the framework about a channel is not free: it inserts a portal row for
// the event's key before any handler runs. A login that never bridged the
// channel must not ask, or a deleted channel leaves an empty portal behind.
func TestQueueLifecycleCheckSkipsChannelsThisLoginNeverBridged(t *testing.T) {
	c := &MattermostClient{UserID: "me", UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}}
	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(evt bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, evt)
		return bridgev2.EventHandlingResultSuccess
	}

	c.hasPortalFn = func(string) bool { return false }
	c.queueLifecycleCheck("channel-x", true)
	c.queueLifecycleCheck("channel-x", false)
	if len(queued) != 0 {
		t.Fatalf("queued %d hints for a channel this login never bridged", len(queued))
	}

	c.hasPortalFn = func(string) bool { return true }
	c.queueLifecycleCheck("channel-x", true)
	if len(queued) != 1 {
		t.Fatalf("queued %d hints for an owned channel, want 1", len(queued))
	}
}

// The hint must be about the channel the caller asked for.
func TestQueueLifecycleCheckChecksTheHintedChannel(t *testing.T) {
	c := &MattermostClient{UserID: "me", UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}}
	c.queueRemoteEventFn = func(bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		return bridgev2.EventHandlingResultSuccess
	}
	var asked []string
	c.hasPortalFn = func(id string) bool { asked = append(asked, id); return false }
	c.queueLifecycleCheck("channel-y", true)

	if len(asked) != 1 || asked[0] != "channel-y" {
		t.Fatalf("asked about %v, want [channel-y]", asked)
	}
}

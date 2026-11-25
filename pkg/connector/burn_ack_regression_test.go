package connector

import (
	"testing"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

func TestBurnMetadataUpdateDoesNotQueueEdit(t *testing.T) {
	var events []bridgev2.RemoteEvent
	feed := make(chan any, 2)
	c := &MattermostClient{UserID: meID, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}, client: &mattermost.Client{Events: feed}, queueRemoteEventFn: func(e bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		events = append(events, e)
		return bridgev2.EventHandlingResultSuccess
	}}
	c.quietReconnect.Store(true)
	feed <- &mattermost.PostEditedEvent{ChannelID: "c1", Post: burnPost(meID, secretText, true)}
	feed <- &mattermost.PostEditedEvent{ChannelID: "c1", Post: &mattermost.Post{ID: "ordinary", UserID: meID, ChannelID: "c1", Message: "edited"}}
	close(feed)
	c.processEvents()
	if len(events) != 1 || events[0].GetType() != bridgev2.RemoteEventEdit {
		t.Fatalf("expected only ordinary edit queued, got %d events", len(events))
	}
}

package connector

import (
	"testing"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
)

func TestLiveReactionSenderAttribution(t *testing.T) {
	for _, actor := range []string{"me", "alice"} {
		for _, add := range []bool{true, false} {
			label := actor + "/remove"
			if add {
				label = actor + "/add"
			}
			t.Run(label, func(t *testing.T) {
				c := reactionClient()
				input := make(chan any, 1)
				c.client = &mattermost.Client{Events: input}
				// Closing the finite test stream is deliberate, not a lost connection.
				c.quietReconnect.Store(true)
				var queued []bridgev2.RemoteEvent
				c.queueRemoteEventFn = func(e bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
					queued = append(queued, e)
					return bridgev2.EventHandlingResultSuccess
				}
				input <- &mattermost.ReactionEvent{ChannelID: "channel", IsAdd: add, Reaction: &mattermost.Reaction{UserID: actor, PostID: "post", EmojiName: "green_circle"}}
				close(input)
				c.processEvents()
				if len(queued) != 1 {
					t.Fatalf("queued %d events, want 1", len(queued))
				}
				got, ok := queued[0].(*simplevent.Reaction)
				if !ok {
					t.Fatalf("queued type %T", queued[0])
				}
				own := actor == c.UserID
				if got.Sender.Sender != networkid.UserID(actor) || got.Sender.IsFromMe != own {
					t.Fatalf("sender = %+v, want actor %q, IsFromMe=%t", got.Sender, actor, own)
				}
				wantLogin := networkid.UserLoginID("")
				if own {
					wantLogin = c.UserLogin.ID
				}
				if got.Sender.SenderLogin != wantLogin {
					t.Fatalf("sender login = %q, want %q", got.Sender.SenderLogin, wantLogin)
				}
				wantType := bridgev2.RemoteEventReactionRemove
				if add {
					wantType = bridgev2.RemoteEventReaction
				}
				if got.GetType() != wantType || got.TargetMessage != "post" || got.EmojiID != "green_circle" || got.Emoji != mmEmojiToUnicode("green_circle") {
					t.Fatalf("reaction content changed: %+v", got)
				}
				if got.PortalKey.ID != "channel" || got.PortalKey.Receiver != c.UserLogin.ID || got.CreatePortal {
					t.Fatalf("portal metadata = %+v", got.EventMeta)
				}
			})
		}
	}
}

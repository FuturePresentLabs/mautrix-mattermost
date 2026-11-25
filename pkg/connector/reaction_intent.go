package connector

import (
	"context"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// reactionIntent is what a Matrix reaction on a bridged post means.
//
// Under time pressure nobody should have to find a particular emoji, so the
// reaction itself is the gesture: any reaction opens someone else's
// burn-on-read post, and any reaction acknowledges someone else's post that
// asks for acknowledgement. Neither is sent to Mattermost as an emoji.
type reactionIntent int

const (
	// reactionOrdinary is an emoji reaction, sent to Mattermost as one.
	reactionOrdinary reactionIntent = iota
	// reactionReveal opens someone else's burn-on-read post.
	reactionReveal
	// reactionAcknowledge acknowledges someone else's post that asks for it.
	reactionAcknowledge
)

// classifyReaction reads the post a reaction is aimed at, once. A post that
// cannot be read counts as ordinary: the worst outcome is then an emoji, never
// a reveal or an acknowledgement nobody asked for. Your own posts are always
// ordinary, because the reveal and the acknowledgement are for recipients.
func (c *MattermostClient) classifyReaction(ctx context.Context, postID string) (reactionIntent, *mattermost.Post) {
	post, err := mattermost.GetPost(c.serverURL(), c.Token, postID)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("post_id", postID).Msg("Could not check what a reaction is aimed at; treating it as an ordinary reaction")
		return reactionOrdinary, nil
	}
	if c.UserID != "" && post.UserID == c.UserID {
		return reactionOrdinary, post
	}
	switch {
	case post.IsBurnOnRead() && !post.RevealedForViewer():
		return reactionReveal, post
	case post.RequestsAcknowledgement():
		// Reveal and acknowledge are separate acts on one message: the first
		// reaction opens it, a later one acknowledges it.
		return reactionAcknowledge, post
	}
	return reactionOrdinary, post
}

// gestureReaction is what the framework records for a reaction that acted as a
// gesture. It is filed under an id of its own. The framework drops a reaction as
// a duplicate when the same user already holds the same emoji on that message,
// so recording the real emoji would make the next gesture with it (reacting 🔥 to
// reveal, then 🔥 again to acknowledge) vanish without ever reaching this code.
// The row goes away with the message.
func gestureReaction(msg *bridgev2.MatrixReaction) *database.Reaction {
	emojiID := msg.PreHandleResp.EmojiID
	if msg.Event != nil {
		emojiID = networkid.EmojiID("gesture:" + string(msg.Event.ID))
	}
	return &database.Reaction{EmojiID: emojiID, Emoji: msg.PreHandleResp.Emoji}
}

// takeBackGesture redacts the reaction that did the job. Left in place it would
// look as though that emoji had been sent as a reaction. Failing to redact never
// fails the gesture: a reveal already happened on the server and cannot be undone.
func (c *MattermostClient) takeBackGesture(ctx context.Context, msg *bridgev2.MatrixReaction) {
	if msg == nil || msg.Event == nil || msg.Portal == nil {
		return
	}
	var err error
	switch {
	case c.redactReactionFn != nil:
		err = c.redactReactionFn(ctx, msg.Portal.MXID, msg.Event.ID)
	case msg.Portal.Bridge != nil && msg.Portal.Bridge.Bot != nil:
		_, err = msg.Portal.Bridge.Bot.SendMessage(ctx, msg.Portal.MXID, event.EventRedaction, &event.Content{
			Parsed: &event.RedactionEventContent{Redacts: msg.Event.ID},
		}, nil)
	default:
		return
	}
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Stringer("event_id", msg.Event.ID).Msg("Could not take back the reaction that revealed or acknowledged a message")
	}
}

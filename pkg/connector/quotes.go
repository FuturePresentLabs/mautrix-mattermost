package connector

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Replies that are not threads.
//
// Mattermost only has threads, but the Channel Reply plugin adds replies to
// one message: a channel post carrying the replied-to post's ID (see
// mattermost/quotes.go). When the server has the plugin active and
// network.quoted_replies is on, a Matrix reply outside a thread is sent as
// such a post, and one inside a thread as a quoted reply in the thread. Both
// ways, those posts arrive in Matrix as replies to the post they quote.
// Without the plugin, replies go to the thread of the message replied to,
// as before.

// pluginCheckTTL is how long the server's answer about the plugin is used.
var pluginCheckTTL = 10 * time.Minute

// quotedRepliesEnabled reports network.quoted_replies and whether the
// server has the plugin active.
func (c *MattermostClient) quotedRepliesEnabled() bool {
	if c.quotedRepliesFn != nil {
		return c.quotedRepliesFn()
	}
	if cfg := bridgeConfig(c.bridge()); cfg == nil || !cfg.QuotedReplies {
		return false
	}
	return c.channelReplyPluginActive()
}

// channelReplyPluginActive asks the server, at most once per pluginCheckTTL.
// An unanswered question counts as no plugin: replies then still reach
// Mattermost, as thread replies.
func (c *MattermostClient) channelReplyPluginActive() bool {
	c.mu.RLock()
	active, checked := c.quotePluginActive, c.quotePluginChecked
	c.mu.RUnlock()
	if !checked.IsZero() && time.Since(checked) < pluginCheckTTL {
		return active
	}
	active, err := mattermost.WebappPluginActive(c.serverURL(), c.Token, mattermost.ChannelReplyPluginID)
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Warn().Err(err).Msg("Could not check for the Channel Reply plugin; sending replies as thread replies")
		active = false
	}
	c.mu.Lock()
	c.quotePluginActive, c.quotePluginChecked = active, time.Now()
	c.mu.Unlock()
	return active
}

// outboundQuote turns a Matrix reply into a quoted reply: the post to send
// and true, or false to send it as before. A reply whose target cannot be
// read is sent as before rather than failing.
func (c *MattermostClient) outboundQuote(ctx context.Context, msg *bridgev2.MatrixMessage, channelID, body string, fileIDs []string) (mattermost.NewPost, bool) {
	if msg == nil || msg.ReplyTo == nil || isCommandMessage(msg.ReplyTo.ID) || !c.quotedRepliesEnabled() {
		return mattermost.NewPost{}, false
	}
	target, err := mattermost.GetPost(c.serverURL(), c.Token, string(msg.ReplyTo.ID))
	if err != nil || target == nil || target.DeleteAt != 0 {
		zerolog.Ctx(ctx).Warn().Err(err).Str("reply_to", string(msg.ReplyTo.ID)).Msg("Could not read the message replied to; sending a thread reply")
		return mattermost.NewPost{}, false
	}
	author := ""
	if u, err := mattermost.GetUser(c.serverURL(), c.Token, target.UserID); err == nil && u != nil {
		author = quoteAuthor(u)
	}
	message, postType, props := mattermost.QuotedReply(target, author, body)
	rootID := ""
	if msg.ThreadRoot != nil {
		rootID = outboundRootID(msg) // a reply inside a thread stays in it
	}
	return mattermost.NewPost{ChannelID: channelID, Message: message, RootID: rootID, FileIDs: fileIDs, Type: postType, Props: props}, true
}

// sendPost creates a post for a Matrix message, as a quoted reply when the
// message is a reply that can be quoted.
func (c *MattermostClient) sendPost(ctx context.Context, msg *bridgev2.MatrixMessage, np mattermost.NewPost) (string, error) {
	if quoted, ok := c.outboundQuote(ctx, msg, np.ChannelID, np.Message, np.FileIDs); ok {
		np = quoted
	}
	return mattermost.SendPostWith(c.serverURL(), c.Token, np)
}

// quoteAuthor names a quoted author as the plugin does: full name, else
// username.
func quoteAuthor(u *mattermost.User) string {
	if full := strings.TrimSpace(u.FirstName + " " + u.LastName); full != "" {
		return full
	}
	return u.Username
}

// quotedReplyTarget is the Matrix reply target of a quoted reply, or nil.
func quotedReplyTarget(post *mattermost.Post) *networkid.MessageOptionalPartID {
	id := post.QuotedReplyTo()
	if id == "" {
		return nil
	}
	return &networkid.MessageOptionalPartID{MessageID: networkid.MessageID(id)}
}

// editQuotedReply edits a quoted reply, keeping its quote. handled is false
// when the post is not a quoted reply (or could not be read), so the caller
// edits it as usual.
func (c *MattermostClient) editQuotedReply(postID, body string) (handled bool, err error) {
	post, err := mattermost.GetPost(c.serverURL(), c.Token, postID)
	if err != nil || post == nil {
		return false, nil
	}
	message, props, ok := mattermost.QuotedReplyEdit(post, body)
	if !ok {
		return false, nil
	}
	return true, mattermost.PatchPost(c.serverURL(), c.Token, postID, message, props)
}

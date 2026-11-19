package mattermost

import (
	"strings"
)

// Quoted replies, as made by the Channel Reply webapp plugin
// (com.github.mattermost-channel-reply): a post that replies to one message
// without starting a thread. Mattermost itself has no such relation, so the
// plugin keeps it in the post: the replied-to post ID and the reply text in
// props, a custom post type its webapp renders, and a Markdown quote block
// before the reply text so other clients (mobile, the API) still read it.

const (
	// ChannelReplyPluginID is the plugin's ID, used to tell whether the
	// server has it active.
	ChannelReplyPluginID = "com.github.mattermost-channel-reply"
	// QuotedReplyPostType is the post type the plugin renders.
	QuotedReplyPostType = "custom_quoted_reply"
	// QuotedReplyToProp holds the ID of the post replied to.
	QuotedReplyToProp = "quoted_reply_to"
	// QuotedReplyBodyProp holds the reply text without the quote block.
	QuotedReplyBodyProp = "quoted_reply_body"

	quoteMaxLength = 500
)

// QuotedReplyTo is the ID of the post a quoted reply answers, or "".
func (p *Post) QuotedReplyTo() string {
	if p == nil {
		return ""
	}
	id, _ := p.Props[QuotedReplyToProp].(string)
	if id == p.ID {
		return ""
	}
	return id
}

// IsQuotedReply reports whether the post is a quoted reply.
func (p *Post) IsQuotedReply() bool {
	return p != nil && (p.Type == QuotedReplyPostType || p.QuotedReplyTo() != "")
}

// QuotedReplyBody is a post's own text: for a quoted reply the message
// without its quote block (which an edit may have changed since the post
// was made, so the message wins over the props), otherwise the message.
func (p *Post) QuotedReplyBody() string {
	if p == nil {
		return ""
	}
	if !p.IsQuotedReply() {
		return p.Message
	}
	if body, ok := stripQuoteBlock(p.Message); ok {
		return body
	}
	if body, ok := p.Props[QuotedReplyBodyProp].(string); ok && body != "" {
		return body
	}
	return p.Message
}

// stripQuoteBlock removes a leading quote block (lines starting "> ") and
// the blank line after it, as the plugin does.
func stripQuoteBlock(message string) (string, bool) {
	if !strings.HasPrefix(message, "> ") {
		return message, false
	}
	sep := strings.Index(message, "\n\n")
	if sep < 0 {
		return message, false
	}
	for _, line := range strings.Split(message[:sep], "\n") {
		if !strings.HasPrefix(line, "> ") {
			return message, false
		}
	}
	return message[sep+2:], true
}

// QuoteBlock is the quote block the plugin puts before a reply: the author
// in bold, then the quoted text collapsed to one line and cut at 500
// characters.
func QuoteBlock(author, quoted string) string {
	author = strings.TrimSpace(author)
	if author == "" {
		author = "Unknown user"
	}
	text := strings.Join(strings.Fields(quoted), " ")
	if text == "" {
		text = "Attachment"
	}
	if r := []rune(text); len(r) > quoteMaxLength {
		text = string(r[:quoteMaxLength-1]) + "…"
	}
	return "> **" + author + "**\n> " + text
}

// QuotedReply builds the message, type and props of a quoted reply to
// target by author, with body as the reply text.
func QuotedReply(target *Post, author, body string) (message, postType string, props map[string]any) {
	message = QuoteBlock(author, target.QuotedReplyBody())
	if body != "" {
		message += "\n\n" + body
	}
	return message, QuotedReplyPostType, map[string]any{
		QuotedReplyToProp:   target.ID,
		QuotedReplyBodyProp: body,
	}
}

// QuotedReplyEdit rebuilds an edited quoted reply: the old quote block with
// the new text. ok is false when post is not a quoted reply.
func QuotedReplyEdit(post *Post, body string) (message string, props map[string]any, ok bool) {
	if !post.IsQuotedReply() {
		return "", nil, false
	}
	quote := post.Message
	if stripped, found := stripQuoteBlock(post.Message); found {
		quote = strings.TrimSuffix(post.Message[:len(post.Message)-len(stripped)], "\n\n")
	} else if !strings.HasPrefix(post.Message, "> ") {
		quote = ""
	}
	message = body
	if quote != "" {
		message = quote + "\n\n" + body
	}
	props = make(map[string]any, len(post.Props)+1)
	for k, v := range post.Props {
		props[k] = v
	}
	props[QuotedReplyBodyProp] = body
	return message, props, true
}

// WebappPluginActive reports whether a webapp plugin is active on the
// server. Any logged-in user may list them.
func WebappPluginActive(serverURL, token, pluginID string) (bool, error) {
	var manifests []struct {
		ID string `json:"id"`
	}
	if err := doGet(serverURL, token, "/api/v4/plugins/webapp", &manifests); err != nil {
		return false, err
	}
	for _, m := range manifests {
		if m.ID == pluginID {
			return true, nil
		}
	}
	return false, nil
}

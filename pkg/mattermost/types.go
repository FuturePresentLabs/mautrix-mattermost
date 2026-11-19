// Package mattermost provides Go types and helpers for the Mattermost REST and WebSocket API.
package mattermost

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Post represents a Mattermost post (message).
type Post struct {
	ID        string `json:"id"`
	CreateAt  int64  `json:"create_at"`
	UpdateAt  int64  `json:"update_at"`
	DeleteAt  int64  `json:"delete_at"`
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	RootID    string `json:"root_id"` // non-empty when this is a thread reply
	// ReplyCount is only set on thread root posts and counts the replies in the thread.
	ReplyCount int           `json:"reply_count"`
	Message    string        `json:"message"`
	Type       string        `json:"type"`
	FileIDs    []string      `json:"file_ids,omitempty"`
	Metadata   *PostMetadata `json:"metadata,omitempty"`
	// Props holds post properties; integrations (Google Meet, calendar and
	// GitHub plugins, webhooks) put their content in props.attachments.
	Props map[string]any `json:"props,omitempty"`
}

// MessageAttachment is a Slack-style attachment card (props.attachments).
type MessageAttachment struct {
	Fallback   string                    `json:"fallback"`
	Pretext    string                    `json:"pretext"`
	AuthorName string                    `json:"author_name"`
	AuthorLink string                    `json:"author_link"`
	Title      string                    `json:"title"`
	TitleLink  string                    `json:"title_link"`
	Text       string                    `json:"text"`
	Fields     []MessageAttachmentField  `json:"fields"`
	ImageURL   string                    `json:"image_url"`
	Footer     string                    `json:"footer"`
	Actions    []MessageAttachmentAction `json:"actions"`
}

// MessageAttachmentField is one titled value in an attachment.
type MessageAttachmentField struct {
	Title string `json:"title"`
	Value any    `json:"value"`
}

// MessageAttachmentAction is an interactive button or menu.
type MessageAttachmentAction struct {
	Name string `json:"name"`
}

// Attachments decodes props.attachments. A field with an unexpected type is
// left empty and the rest is kept (an integration sending a number where
// Mattermost expects text should still show); anything else that does not
// decode is ignored, so a broken integration cannot fail the post.
func (p *Post) Attachments() []MessageAttachment {
	raw, ok := p.Props["attachments"]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []MessageAttachment
	if err := json.Unmarshal(data, &out); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return nil
		}
	}
	return out
}

// Timestamp returns the post creation time.
func (p *Post) Timestamp() time.Time {
	return time.UnixMilli(p.CreateAt)
}

// PostMetadata holds optional rich metadata on a post.
type PostMetadata struct {
	Files     []*FileInfo `json:"files,omitempty"`
	Reactions []*Reaction `json:"reactions,omitempty"`
	// Embeds are the server's link previews (type "opengraph").
	Embeds []*PostEmbed `json:"embeds,omitempty"`
	// Images holds the size of images the post refers to, keyed by URL.
	Images map[string]*PostImage `json:"images,omitempty"`
}

// PostEmbed is something the server unfurled for a post.
type PostEmbed struct {
	Type string          `json:"type"`
	URL  string          `json:"url"`
	Data json.RawMessage `json:"data,omitempty"`
}

// OpenGraph is an "opengraph" embed's data.
type OpenGraph struct {
	URL         string           `json:"url"`
	Type        string           `json:"type"`
	Title       string           `json:"title"`
	Description string           `json:"description"`
	SiteName    string           `json:"site_name"`
	Images      []OpenGraphImage `json:"images"`
}

// OpenGraphImage is an image of a link preview.
type OpenGraphImage struct {
	URL       string `json:"url"`
	SecureURL string `json:"secure_url"`
	Type      string `json:"type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

// PostImage is an image's size as the server measured it.
type PostImage struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Format string `json:"format"`
}

// IsSystem reports whether the post is a server message ("alice joined the
// channel"), not something a user wrote.
func (p *Post) IsSystem() bool {
	return p != nil && strings.HasPrefix(p.Type, "system_")
}

// FileInfo describes a file attached to a post.
type FileInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	MimeType  string `json:"mime_type"`
	Extension string `json:"extension"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

// User is a Mattermost user profile.
type User struct {
	ID             string `json:"id"`
	Username       string `json:"username"`
	Email          string `json:"email"`
	FirstName      string `json:"first_name"`
	LastName       string `json:"last_name"`
	Nickname       string `json:"nickname"`
	ProfilePicture string `json:"-"` // resolved from /api/v4/users/{id}/image
	// Props holds free-form profile properties, including "customStatus".
	// Values are decoded as any: a non-string value must not fail the whole
	// profile.
	Props map[string]any `json:"props,omitempty"`
	// IsBot marks bot accounts, which Mattermost shows without a status.
	IsBot bool `json:"is_bot,omitempty"`
}

// CustomStatus is a user's Mattermost custom status (emoji and text).
type CustomStatus struct {
	Emoji     string    `json:"emoji"`
	Text      string    `json:"text"`
	Duration  string    `json:"duration"`
	ExpiresAt time.Time `json:"expires_at"`
}

// defaultCustomStatusEmoji is what Mattermost shows for a custom status that
// has text but no emoji.
const defaultCustomStatusEmoji = "speech_balloon"

// ActiveCustomStatus returns the user's custom status, or nil when none is
// set or it has expired at now. As in Mattermost, a status with an empty
// duration ("don't clear") never expires, and a status with text but no emoji
// shows the speech balloon.
func (u *User) ActiveCustomStatus(now time.Time) *CustomStatus {
	raw, _ := u.Props["customStatus"].(string)
	if raw == "" {
		return nil
	}
	var cs CustomStatus
	if json.Unmarshal([]byte(raw), &cs) != nil {
		return nil
	}
	if cs.Emoji == "" && cs.Text == "" {
		return nil
	}
	if cs.Duration != "" && !cs.ExpiresAt.IsZero() && !now.Before(cs.ExpiresAt) {
		return nil
	}
	if cs.Emoji == "" {
		cs.Emoji = defaultCustomStatusEmoji
	}
	return &cs
}

// DisplayName returns the most human-readable name available.
func (u *User) DisplayName() string {
	return u.NameFor(NameFormatNicknameFullName)
}

// Mattermost's name formats, as used by the TeammateNameDisplay setting and
// the per-user "name_format" display preference.
const (
	NameFormatUsername         = "username"
	NameFormatNicknameFullName = "nickname_full_name"
	NameFormatFullName         = "full_name"
)

// FullName is "First Last" without stray spaces; empty when neither is set.
func (u *User) FullName() string {
	return strings.TrimSpace(u.FirstName + " " + u.LastName)
}

// NameFor returns the user's name as Mattermost shows it under the given name
// format, with Mattermost's fallbacks: a missing nickname falls back to the
// full name, and a missing full name to the username. Unknown formats are
// treated as username, Mattermost's default.
func (u *User) NameFor(format string) string {
	switch format {
	case NameFormatNicknameFullName:
		if u.Nickname != "" {
			return u.Nickname
		}
		fallthrough
	case NameFormatFullName:
		if n := u.FullName(); n != "" {
			return n
		}
	}
	return u.Username
}

// Channel represents a Mattermost channel.
// Type: "O" = public, "P" = private, "D" = direct (1:1), "G" = group DM.
type Channel struct {
	ID          string `json:"id"`
	TeamID      string `json:"team_id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	Header      string `json:"header"`
	Purpose     string `json:"purpose"`
	// DeleteAt is the soft-deletion (archive) timestamp in milliseconds, 0 when active.
	DeleteAt int64 `json:"delete_at"`
}

// IsDM returns true for 1:1 direct message channels.
func (ch *Channel) IsDM() bool { return ch.Type == "D" }

// IsGroupDM returns true for group DM channels.
func (ch *Channel) IsGroupDM() bool { return ch.Type == "G" }

// Team is a Mattermost team.
type Team struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	// LastTeamIconUpdate is when the team icon last changed (ms); 0 means
	// the team has no icon.
	LastTeamIconUpdate int64 `json:"last_team_icon_update"`
}

// ChannelMember represents a user's membership in a channel, with their
// notification preferences for it.
type ChannelMember struct {
	ChannelID   string            `json:"channel_id"`
	UserID      string            `json:"user_id"`
	Roles       string            `json:"roles"`
	NotifyProps map[string]string `json:"notify_props,omitempty"`
	// LastViewedAt is when the user last viewed the channel (ms).
	LastViewedAt int64 `json:"last_viewed_at,omitempty"`
}

// Reaction is an emoji reaction attached to a post.
type Reaction struct {
	UserID    string `json:"user_id"`
	PostID    string `json:"post_id"`
	EmojiName string `json:"emoji_name"`
	CreateAt  int64  `json:"create_at"`
	// DeleteAt is non-zero for a removed reaction. Posts normally omit those.
	DeleteAt int64 `json:"delete_at,omitempty"`
}

// PostList is the response from the paginated posts endpoint.
type PostList struct {
	Order      []string         `json:"order"`
	Posts      map[string]*Post `json:"posts"`
	NextPostID string           `json:"next_post_id"`
	PrevPostID string           `json:"prev_post_id"`
}

// --- WebSocket event types ---

// WSEvent is the raw envelope for Mattermost WebSocket events.
type WSEvent struct {
	Event     string                 `json:"event"`
	Data      map[string]interface{} `json:"data"`
	Broadcast WSBroadcast            `json:"broadcast"`
	Seq       int64                  `json:"seq"`
}

// WSBroadcast holds routing metadata for a WS event.
type WSBroadcast struct {
	OmitUsers map[string]bool `json:"omit_users,omitempty"`
	UserID    string          `json:"user_id"`
	ChannelID string          `json:"channel_id"`
	TeamID    string          `json:"team_id"`
}

// WSAuthResponse is the server's reply to an authentication_challenge action.
type WSAuthResponse struct {
	Status   string `json:"status"`
	SeqReply int64  `json:"seq_reply"`
}

// HelloEvent is published by the server when a WS connection is established.
type HelloEvent struct {
	ServerVersion string `json:"server_version"`
}

// ChannelMemberUpdatedEvent carries a change to the user's membership of a
// channel, such as muting it.
type ChannelMemberUpdatedEvent struct {
	Member *ChannelMember
}

// EphemeralEvent carries a post shown only to this user and never stored,
// such as a slash command's reply.
type EphemeralEvent struct {
	Post *Post
}

// PostedEvent carries a newly created post.
type PostedEvent struct {
	Post      *Post
	ChannelID string
	TeamID    string
	// Channel metadata sent inline by Mattermost – avoids an extra REST round-trip.
	ChannelType string // "D" = DM, "G" = group DM, "O" = public, "P" = private
	ChannelName string // raw channel name, e.g. "userId1__userId2" for DMs
}

// PostEditedEvent carries the updated post.
type PostEditedEvent struct {
	Post      *Post
	ChannelID string
}

// PostDeletedEvent carries the (soft-)deleted post.
type PostDeletedEvent struct {
	Post      *Post
	ChannelID string
}

// ReactionEvent carries a reaction add or remove.
type ReactionEvent struct {
	Reaction  *Reaction
	ChannelID string
	IsAdd     bool
}

// TypingEvent indicates a user is typing in a channel.
type TypingEvent struct {
	UserID    string
	ChannelID string
}

// ChannelCreatedEvent signals a new channel was created.
type ChannelCreatedEvent struct {
	Channel *Channel
}

// ChannelUpdatedEvent signals a channel was updated.
type ChannelUpdatedEvent struct {
	Channel *Channel
}

// ChannelDeletedEvent signals a channel was deleted.
type ChannelDeletedEvent struct {
	ChannelID string
	TeamID    string
}

// ChannelRestoredEvent signals a channel was unarchived.
type ChannelRestoredEvent struct {
	ChannelID string
	TeamID    string
}

// ChannelMemberEvent signals that a user joined or left a channel
// ("user_added" / "user_removed").
type ChannelMemberEvent struct {
	ChannelID string
	UserID    string
	Added     bool
}

// DirectAddedEvent signals a DM channel was opened with the local user.
type DirectAddedEvent struct {
	ChannelID string
}

// StatusChangeEvent signals a user's online status changed.
type StatusChangeEvent struct {
	UserID string
	Status string
}

// UserUpdatedEvent signals a user profile changed.
type UserUpdatedEvent struct {
	User *User
}

// TeamUpdatedEvent signals a team's name or icon changed.
type TeamUpdatedEvent struct {
	Team *Team
}

// ChannelsViewedEvent signals the logged-in user read channels in Mattermost.
// Times maps channel ID to the view time in milliseconds; 0 means "now"
// (the server did not say).
type ChannelsViewedEvent struct {
	Times map[string]int64
}

// ThreadReadEvent is a collapsed reply thread read by the user
// ("thread_read_changed"), up to Timestamp (ms).
type ThreadReadEvent struct {
	ThreadID  string
	ChannelID string
	Timestamp int64
}

// AuthErrorEvent signals the WS auth was rejected or a session was invalidated.
type AuthErrorEvent struct {
	Message string
}

package mattermost

import (
	"net/url"
	"strconv"
	"time"
)

// PostTypeBurnOnRead is the type of a burn-on-read post. The server keeps its
// text apart from the post: recipients see an empty body until they reveal it,
// and the author's own sessions receive the text.
const PostTypeBurnOnRead = "burn_on_read"

// IsBurnOnRead reports whether the post is a burn-on-read post. Post.IsSystem
// does not cover it (the type has no "system_" prefix), so every path that
// handles post content has to ask this explicitly.
func (p *Post) IsBurnOnRead() bool {
	return p != nil && p.Type == PostTypeBurnOnRead
}

// BurnExpiresAt is when the post is deleted whether or not anyone reveals it
// (props.expire_at), or the zero time when the server did not say.
func (p *Post) BurnExpiresAt() time.Time {
	return millisProp(p.Props["expire_at"])
}

// BurnReadDuration is how long a recipient has after revealing the post
// (props.read_duration), or 0 when the server did not say.
func (p *Post) BurnReadDuration() time.Duration {
	ms, ok := p.Props["read_duration"].(float64)
	if !ok || ms <= 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// RevealExpiresAt is when this user's revealed copy expires, or the zero time
// if it has not been revealed.
func (p *Post) RevealExpiresAt() time.Time {
	if p.Metadata == nil || p.Metadata.ExpireAt <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(p.Metadata.ExpireAt)
}

func millisProp(v any) time.Time {
	ms, ok := v.(float64)
	if !ok || ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(ms))
}

// RevealPost reveals a burn-on-read post for the token's user and returns it
// with its text. This is explicit by design: it starts that user's countdown
// and tells the author the post was read, so it is only ever called on the
// user's own instruction.
func RevealPost(serverURL, token, postID string) (*Post, error) {
	var p Post
	if err := doGet(serverURL, token, "/api/v4/posts/"+url.PathEscape(postID)+"/reveal", &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// BurnPost burns a burn-on-read post for the token's user: the author's
// post is deleted, a recipient's copy expires at once.
func BurnPost(serverURL, token, postID string) error {
	return doDelete(serverURL, token, "/api/v4/posts/"+url.PathEscape(postID)+"/burn")
}

// RevealedForViewer reports whether the user the post was fetched for has
// already revealed it. The server answers per requesting user: an unrevealed
// burn-on-read post comes back with an empty message and its metadata wiped,
// a revealed one with its text and the expiry of that user's copy.
func (p *Post) RevealedForViewer() bool {
	if p == nil {
		return false
	}
	return p.Message != "" || (p.Metadata != nil && p.Metadata.ExpireAt != 0)
}

// BurnOnReadSettings is the server's burn-on-read configuration as seen by clients.
type BurnOnReadSettings struct {
	Enabled bool
	// Duration is how long a revealed post lasts. Always set when Enabled.
	Duration time.Duration
	// MaxTTL is the longest a post may exist, read or not. Zero means the
	// server did not say, and callers must not substitute a guess.
	MaxTTL time.Duration
}

// GetBurnOnReadSettings reads the burn-on-read settings from the client config.
// A server without the feature, or reporting no usable duration, is not enabled.
func GetBurnOnReadSettings(serverURL, token string) (BurnOnReadSettings, error) {
	var cfg map[string]string
	if err := doGet(serverURL, token, "/api/v4/config/client?format=old", &cfg); err != nil {
		return BurnOnReadSettings{}, err
	}
	secs, err := strconv.Atoi(cfg["BurnOnReadDurationSeconds"])
	if cfg["EnableBurnOnRead"] != "true" || err != nil || secs <= 0 {
		return BurnOnReadSettings{}, nil
	}
	settings := BurnOnReadSettings{Enabled: true, Duration: time.Duration(secs) * time.Second}
	// The longest a post may exist is its own server setting. Leave it unknown
	// (zero) when absent or unusable rather than guess a value.
	if ttl, err := strconv.Atoi(cfg["BurnOnReadMaximumTimeToLiveSeconds"]); err == nil && ttl > 0 {
		settings.MaxTTL = time.Duration(ttl) * time.Second
	}
	return settings, nil
}

// PostRevealedEvent says a burn-on-read post was revealed. The server sends it
// to every session of the revealing user, bridge included, with the whole post.
// Only identifiers and the expiry are kept: the text and files are dropped here
// so no later code can leak them.
type PostRevealedEvent struct {
	PostID    string
	ChannelID string
	// AuthorID is the post's author. An event for the author's own post is a
	// recipient-count update, not a reveal by this user.
	AuthorID string
	// ExpireAt is when the revealing user's copy expires (ms since epoch).
	ExpireAt int64
}

// PostBurnedEvent says a burn-on-read post was burned for this user.
type PostBurnedEvent struct {
	PostID    string
	ChannelID string
}

// PostAllRevealedEvent tells the author every recipient has revealed a
// burn-on-read post; the author's own copy then ends at SenderExpireAt.
type PostAllRevealedEvent struct {
	PostID         string
	ChannelID      string
	SenderExpireAt int64 // ms since epoch
}

package mattermost

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
)

// Acknowledgement is Mattermost's acknowledgement of a single post by a user.
type Acknowledgement struct {
	UserID         string `json:"user_id"`
	PostID         string `json:"post_id"`
	AcknowledgedAt int64  `json:"acknowledged_at"`
	ChannelID      string `json:"channel_id"`
	RemoteID       string `json:"remote_id,omitempty"`
}

// AcknowledgementEvent carries an add or removal. Both use the timestamp of
// the original acknowledgement.
type AcknowledgementEvent struct {
	Acknowledgement
	IsAdd bool
}

func decodeAcknowledgement(env WSEvent) *Acknowledgement {
	raw := strFromData(env.Data, "acknowledgement")
	var ack Acknowledgement
	if json.Unmarshal([]byte(raw), &ack) != nil || ack.AcknowledgedAt <= 0 {
		return nil
	}
	channel := env.Broadcast.ChannelID
	if ack.ChannelID == "" {
		ack.ChannelID = channel
	} else if channel != "" && channel != ack.ChannelID {
		return nil
	}
	for _, id := range []string{ack.UserID, ack.PostID, ack.ChannelID} {
		if id == "" || strings.ContainsFunc(id, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r)
		}) {
			return nil
		}
	}
	return &ack
}

// GetPostForAcknowledgements requests the single target through the post-list
// preparation path. The single-post endpoint only enriches top-level posts
// with acknowledgement metadata. A nil post with no error means the server
// explicitly identified the target as missing.
func GetPostForAcknowledgements(serverURL, token, postID string) (*Post, error) {
	path := "/api/v4/posts/" + postID + "/thread?skipFetchThreads=true"
	var list PostList
	if err := doGet(serverURL, token, path, &list); err != nil {
		var response *HTTPError
		if errors.As(err, &response) && response.StatusCode == http.StatusNotFound &&
			response.Path == path && response.ErrorID == "app.post.get.app_error" &&
			(response.BodyStatusCode == 0 || response.BodyStatusCode == http.StatusNotFound) {
			return nil, nil
		}
		return nil, err
	}
	post := list.Posts[postID]
	if post == nil || post.ID != postID {
		return nil, errors.New("acknowledgement snapshot omitted the requested post")
	}
	return post, nil
}

// GetPostsByIDsForAcknowledgements fetches root snapshots in one request.
// Mattermost enriches only top-level posts with acknowledgements on this route.
func GetPostsByIDsForAcknowledgements(serverURL, token string, postIDs []string) ([]*Post, error) {
	var posts []*Post
	if err := doPost(serverURL, token, "/api/v4/posts/ids", postIDs, &posts); err != nil {
		return nil, err
	}
	return posts, nil
}

// RequestsAcknowledgement reports whether the author asked recipients to
// acknowledge the post. Replies never do: priority belongs to top-level posts.
func (p *Post) RequestsAcknowledgement() bool {
	return p != nil && p.Metadata != nil && p.Metadata.Priority != nil &&
		p.Metadata.Priority.RequestedAck != nil && *p.Metadata.Priority.RequestedAck
}

// AcknowledgedBy reports whether the user has an active acknowledgement on the
// post, according to the metadata the server attached.
func (p *Post) AcknowledgedBy(userID string) bool {
	if p == nil || p.Metadata == nil || userID == "" {
		return false
	}
	for _, ack := range p.Metadata.Acknowledgements {
		if ack != nil && ack.UserID == userID && ack.AcknowledgedAt > 0 {
			return true
		}
	}
	return false
}

// ReactedBy reports whether the user has a live reaction with this emoji name
// on the post, according to the metadata the server attached.
func (p *Post) ReactedBy(userID, emojiName string) bool {
	if p == nil || p.Metadata == nil || userID == "" || emojiName == "" {
		return false
	}
	for _, r := range p.Metadata.Reactions {
		if r != nil && r.UserID == userID && r.EmojiName == emojiName && r.DeleteAt == 0 {
			return true
		}
	}
	return false
}

// AcknowledgePost records the user's acknowledgement of a post. Withdrawing is
// left to Mattermost, which owns the rule for how long that stays possible.
func AcknowledgePost(serverURL, token, userID, postID string) error {
	return doPost(serverURL, token, fmt.Sprintf("/api/v4/users/%s/posts/%s/ack", userID, postID), struct{}{}, nil)
}

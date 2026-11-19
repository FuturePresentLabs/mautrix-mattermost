package mattermost

import (
	"errors"
	"fmt"
	"net/http"
)

// UserThread is a collapsed reply thread the user follows, with their read
// state in it.
type UserThread struct {
	ID            string `json:"id"` // the root post ID
	ReplyCount    int64  `json:"reply_count"`
	LastReplyAt   int64  `json:"last_reply_at"`
	LastViewedAt  int64  `json:"last_viewed_at"`
	UnreadReplies int64  `json:"unread_replies"`
	Post          struct {
		ChannelID string `json:"channel_id"`
	} `json:"post"`
}

// maxThreadPages bounds GetUserThreads: the newest threads are the ones a
// client shows as unread.
const maxThreadPages = 5

// GetUserThreads lists the threads the user follows in a team, newest
// activity first, up to maxThreadPages pages of 100.
func GetUserThreads(serverURL, token, teamID string) ([]*UserThread, error) {
	return getUserThreads(serverURL, token, teamID, "")
}

// GetUnreadThreadIDs lists the root post IDs of the threads Mattermost
// counts as unread for the user in a team.
func GetUnreadThreadIDs(serverURL, token, teamID string) ([]string, error) {
	threads, err := getUserThreads(serverURL, token, teamID, "&unread=true")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(threads))
	for _, th := range threads {
		if th != nil && th.ID != "" {
			ids = append(ids, th.ID)
		}
	}
	return ids, nil
}

func getUserThreads(serverURL, token, teamID, filter string) ([]*UserThread, error) {
	var out []*UserThread
	for page := 0; page < maxThreadPages; page++ {
		var resp struct {
			Total   int64         `json:"total"`
			Threads []*UserThread `json:"threads"`
		}
		path := fmt.Sprintf("/api/v4/users/me/teams/%s/threads?per_page=100&page=%d%s", teamID, page, filter)
		if err := doGet(serverURL, token, path, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Threads...)
		if len(resp.Threads) < 100 || int64(len(out)) >= resp.Total {
			break
		}
	}
	return out, nil
}

// CollapsedThreads reports whether the user sees collapsed reply threads:
// the server setting, and for the default_on/default_off settings the
// user's own preference when set. With them off, replies show in the
// channel, so viewing a channel reads its threads too.
func CollapsedThreads(serverURL, token string) (bool, error) {
	var cfg map[string]any
	if err := doGet(serverURL, token, "/api/v4/config/client?format=old", &cfg); err != nil {
		return false, err
	}
	setting, _ := cfg["CollapsedThreads"].(string)
	switch setting {
	case "always_on":
		return true, nil
	case "default_on", "default_off":
	default: // "disabled", or a server too old to have them
		return false, nil
	}
	var pref struct {
		Value string `json:"value"`
	}
	err := doGet(serverURL, token, "/api/v4/users/me/preferences/display_settings/name/collapsed_reply_threads", &pref)
	var httpErr *HTTPError
	if errors.As(err, &httpErr) && (httpErr.ErrorID == "app.preference.get.app_error" || httpErr.StatusCode == http.StatusNotFound) {
		return setting == "default_on", nil
	}
	if err != nil {
		return false, err
	}
	switch pref.Value {
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return setting == "default_on", nil
}

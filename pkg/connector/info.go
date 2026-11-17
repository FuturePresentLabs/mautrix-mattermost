package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// GetChatInfo returns Matrix room metadata for a Mattermost channel.
func (c *MattermostClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	if teamID, ok := teamIDFromPortal(portal.ID); ok {
		return c.teamChatInfo(teamID)
	}
	chID := string(portal.ID)

	c.mu.RLock()
	ch, ok := c.channels[chID]
	c.mu.RUnlock()

	// Always fetch fresh data so we have up-to-date display name + type.
	// Archive state is reconciled separately through the journal.
	if fresh, err := mattermost.GetChannel(c.serverURL(), c.Token, chID); err == nil {
		ch = fresh
		ok = true
		c.mu.Lock()
		if c.channels == nil {
			c.channels = make(map[string]*mattermost.Channel)
		}
		c.channels[chID] = ch
		c.mu.Unlock()
	}

	roomType := database.RoomTypeDefault
	var name *string

	if ok {
		switch {
		case ch.IsDM():
			roomType = database.RoomTypeDM
		case ch.IsGroupDM():
			roomType = database.RoomTypeGroupDM
		}

		if ch.DisplayName != "" {
			n := ch.DisplayName
			name = &n
		}
	}

	// Fallback name.
	if name == nil {
		n := chID
		name = &n
	}

	var members *bridgev2.ChatMemberList
	if ok {
		members = c.chatMembers(ctx, ch)
	} else {
		members = c.chatMembers(ctx, nil)
	}

	info := &bridgev2.ChatInfo{
		Name:    name,
		Type:    &roomType,
		Members: members,
	}
	if ok {
		info.ParentID = channelParent(ch)
	}
	// A DM is titled after the other person, with their status (dmstatus.go).
	if ok {
		if title, topic := c.dmRoomInfo(ch); title != nil {
			info.Name, info.Topic = title, topic
			info.ExcludeChangesFromTimeline = true
			c.noteDMRoomInfo(ch.ID, title, topic)
		}
	}
	return info, nil
}

// GetUserInfo returns Matrix ghost metadata for a Mattermost user.
func (c *MattermostClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	userID := string(ghost.ID)

	c.mu.RLock()
	u, ok := c.users[userID]
	c.mu.RUnlock()

	if !ok {
		fetched, err := mattermost.GetUser(c.serverURL(), c.Token, userID)
		if err != nil {
			return &bridgev2.UserInfo{}, nil
		}
		u = fetched
		c.mu.Lock()
		if c.users == nil {
			c.users = make(map[string]*mattermost.User)
		}
		c.users[userID] = u
		c.mu.Unlock()
	}

	return c.userInfo(u), nil
}

// userInfo is the ghost profile for a Mattermost user. GetUserInfo, member
// syncs and user_updated events all use it, so a ghost's name is always the
// same function of the Mattermost profile.
func (c *MattermostClient) userInfo(u *mattermost.User) *bridgev2.UserInfo {
	name := c.displayName(u)
	info := &bridgev2.UserInfo{Name: &name}

	// Profile picture: fetched via the authenticated image endpoint. The ID
	// is stable per user, so repeating it does not re-upload the image.
	avatarURL := mattermost.ProfileImageURL(c.serverURL(), u.ID)
	token := c.Token
	info.Avatar = &bridgev2.Avatar{
		ID: networkid.AvatarID(u.ID + "_avatar"),
		Get: func(ctx context.Context) ([]byte, error) {
			return fetchImage(ctx, avatarURL, token)
		},
	}
	return info
}

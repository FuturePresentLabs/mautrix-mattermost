package mattermost

import "fmt"

// Muted reports whether the channel is muted: Mattermost's "Mute channel"
// sets mark_unread to "mention".
func (m *ChannelMember) Muted() bool {
	return m != nil && m.NotifyProps["mark_unread"] == "mention"
}

// GetMyChannelMembers returns the user's memberships in a team, DMs and group
// DMs included.
func GetMyChannelMembers(serverURL, token, teamID string) ([]*ChannelMember, error) {
	var out []*ChannelMember
	if err := doGet(serverURL, token, fmt.Sprintf("/api/v4/users/me/teams/%s/channels/members", teamID), &out); err != nil {
		return nil, err
	}
	return out, nil
}

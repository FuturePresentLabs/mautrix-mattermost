package connector

import (
	"context"
	"strconv"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// One Matrix space per Mattermost team, as the Slack bridge does per
// workspace. A team's channels are children of its space, so two teams'
// "Town Square" rooms are told apart by the space they are in. The space
// carries the team's name and icon. DMs and group DMs belong to no team and
// stay outside the team spaces.
//
// A space is a portal whose ID is teamPortalPrefix + team ID. Mattermost IDs
// never contain ':', so it cannot collide with a channel ID. Code that treats
// a portal ID as a channel ID must skip space portals (isTeamPortal).

const teamPortalPrefix = "team:"

func teamPortalID(teamID string) networkid.PortalID {
	return networkid.PortalID(teamPortalPrefix + teamID)
}

// teamPortalKey matches the framework's channel-parent lookup.
func (c *MattermostClient) teamPortalKey(teamID string) networkid.PortalKey {
	key := networkid.PortalKey{ID: teamPortalID(teamID)}
	if c.UserLogin != nil {
		if br := c.UserLogin.Bridge; br != nil && br.Config != nil && br.Config.SplitPortals {
			key.Receiver = c.receiverID()
		}
	}
	return key
}

// teamIDFromPortal returns the team ID of a team space portal ID.
func teamIDFromPortal(id networkid.PortalID) (string, bool) {
	s := string(id)
	if !strings.HasPrefix(s, teamPortalPrefix) || len(s) == len(teamPortalPrefix) {
		return "", false
	}
	return s[len(teamPortalPrefix):], true
}

func isTeamPortal(id networkid.PortalID) bool {
	_, ok := teamIDFromPortal(id)
	return ok
}

// channelParent is the space a channel belongs to, or nil for DMs and group
// DMs.
func channelParent(ch *mattermost.Channel) *networkid.PortalID {
	if ch == nil || ch.TeamID == "" || ch.IsDM() || ch.IsGroupDM() {
		return nil
	}
	return ptr.Ptr(teamPortalID(ch.TeamID))
}

// teamSpacesEnabled reports network.team_spaces.
func (c *MattermostClient) teamSpacesEnabled() bool {
	if c.teamSpacesFn != nil {
		return c.teamSpacesFn()
	}
	if c.UserLogin != nil && c.UserLogin.Bridge != nil {
		if conn, ok := c.UserLogin.Bridge.Network.(*MattermostConnector); ok && conn.Config != nil {
			return conn.Config.TeamSpaces
		}
	}
	return false
}

// roomParent is the parent to set on a channel's room. With team spaces off
// it is explicitly none, which also moves a room out of a team space it was
// put in while they were on.
func (c *MattermostClient) roomParent(ch *mattermost.Channel) *networkid.PortalID {
	if c.teamSpacesEnabled() {
		return channelParent(ch)
	}
	if ch == nil || ch.TeamID == "" || ch.IsDM() || ch.IsGroupDM() {
		return nil // never had a team space
	}
	return ptr.Ptr(networkid.PortalID(""))
}

// syncTeams creates or updates the spaces of the teams this login bridges,
// and deletes the spaces of the others: teams switched off, or all teams with
// team spaces off (a delete for a space that was never created is ignored).
// A space is deleted only once no room has it as parent: rooms leave it
// through their own resyncs, which run on other queues, and deleting a parent
// first fails. The next sync retries.
func (c *MattermostClient) syncTeams(teams []*mattermost.Team) {
	for _, team := range teams {
		if c.teamSpacesEnabled() && c.ownsTeam(team.ID) {
			c.queueTeamResync(team)
			continue
		}
		if c.spaceHasChildren(team.ID) {
			continue
		}
		c.clearSpaceParents(team.ID)
		c.queueEvent(&mmChatDeleteEvent{
			portalKey: c.teamPortalKey(team.ID),
		})
	}
}

// spaceHasChildren reports whether any room still has the team space as
// parent. When the database cannot be asked, it says yes, so nothing is
// deleted on a guess.
func (c *MattermostClient) spaceHasChildren(teamID string) bool {
	if c.spaceChildrenFn != nil {
		return c.spaceChildrenFn(teamID)
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DB == nil {
		return true
	}
	// The parent key has a receiver only with split portals; check both.
	for _, receiver := range []networkid.UserLoginID{c.UserLogin.ID, ""} {
		children, err := c.UserLogin.Bridge.DB.Portal.GetChildren(context.Background(), networkid.PortalKey{ID: teamPortalID(teamID), Receiver: receiver})
		if err != nil || len(children) > 0 {
			return true
		}
	}
	return false
}

// teamInfo is a team space's room info: the team's name and icon, the user
// as the only member, and nobody but the bridge allowed to post.
func (c *MattermostClient) teamInfo(team *mattermost.Team) *bridgev2.ChatInfo {
	name := team.DisplayName
	if name == "" {
		name = team.Name
	}
	self := bridgev2.EventSender{IsFromMe: true, SenderLogin: c.receiverID(), Sender: networkid.UserID(c.UserID)}
	return &bridgev2.ChatInfo{
		Name:   &name,
		Avatar: c.teamAvatar(team),
		Type:   ptr.Ptr(database.RoomTypeSpace),
		Members: &bridgev2.ChatMemberList{
			MemberMap:   map[networkid.UserID]bridgev2.ChatMember{self.Sender: {EventSender: self}},
			PowerLevels: &bridgev2.PowerLevelOverrides{EventsDefault: ptr.Ptr(100)},
		},
	}
}

// teamAvatar is the team icon. Its ID changes with the icon, so a new icon
// replaces the old one and an unchanged icon is not fetched again.
func (c *MattermostClient) teamAvatar(team *mattermost.Team) *bridgev2.Avatar {
	if team.LastTeamIconUpdate <= 0 {
		return &bridgev2.Avatar{Remove: true}
	}
	url := mattermost.TeamImageURL(c.serverURL(), team.ID)
	token := c.Token
	return &bridgev2.Avatar{
		ID: networkid.AvatarID("team_" + team.ID + "_" + strconv.FormatInt(team.LastTeamIconUpdate, 10)),
		Get: func(ctx context.Context) ([]byte, error) {
			return fetchImage(ctx, url, token)
		},
	}
}

// teamChatInfo answers GetChatInfo for a team space portal.
func (c *MattermostClient) teamChatInfo(teamID string) (*bridgev2.ChatInfo, error) {
	var team *mattermost.Team
	var err error
	if c.getTeamFn != nil {
		team, err = c.getTeamFn(teamID)
	} else {
		team, err = mattermost.GetTeam(c.serverURL(), c.Token, teamID)
	}
	if err != nil {
		return nil, err
	}
	return c.teamInfo(team), nil
}

// queueTeamResync creates or updates a team's space (name and icon).
func (c *MattermostClient) queueTeamResync(team *mattermost.Team) {
	if !c.teamSpacesEnabled() {
		return
	}
	info := c.teamInfo(team)
	c.queueEvent(&simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			CreatePortal: true,
			PortalKey:    c.teamPortalKey(team.ID),
		},
		ChatInfo: info,
	})
}

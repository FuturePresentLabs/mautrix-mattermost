package connector

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Connect-time upkeep for team logins (see teamlogin.go).

// ownsPost decides a posted event from its inline channel type and team.
// Mattermost always sends both; without them the post is kept, as before
// logins had teams.
func (c *MattermostClient) ownsPost(evt *mattermost.PostedEvent) bool {
	if evt.ChannelType == "" {
		return true
	}
	return c.ownsPostedChannel(evt.ChannelType, evt.TeamID)
}

// ownTeams is the teams this login bridges: its team, or all of them for a
// login without one.
func (c *MattermostClient) ownTeams(teams []*mattermost.Team) []*mattermost.Team {
	scope := c.teamScope()
	if scope == "" {
		return teams
	}
	for _, t := range teams {
		if t.ID == scope {
			return []*mattermost.Team{t}
		}
	}
	return nil
}

// pinLegacyLogin gives a login made before teams were per login its team:
// the user's first team (teamForLegacyLogin). It keeps the DMs.
func (c *MattermostClient) pinLegacyLogin(teams []*mattermost.Team) {
	lg := c.lifecycleLogger()
	meta := c.loginMeta()
	if meta == nil || meta.TeamID != "" {
		return
	}
	team := teamForLegacyLogin(teams)
	if team == nil {
		return
	}
	meta.TeamID = team.ID
	meta.TeamName = teamLabel(team, teams)
	meta.DMs = true
	lg.Info().Str("team_id", team.ID).Str("team", meta.TeamName).
		Msg("Pinned this login to its first team; other teams need their own login")
	c.saveLogin()
}

// syncLoginProfile names the login after its team and uses the team icon as
// its avatar and its space's avatar. The icon is uploaded once per change.
func (c *MattermostClient) syncLoginProfile(teams []*mattermost.Team) {
	lg := c.lifecycleLogger()
	meta := c.loginMeta()
	own := c.ownTeams(teams)
	if meta == nil || meta.TeamID == "" || len(own) != 1 {
		return
	}
	team := own[0]
	label := teamLabel(team, teams)
	ul := c.UserLogin
	changed := false
	if meta.TeamName != label || ul.RemoteName != label || ul.RemoteProfile.Name != label {
		meta.TeamName, ul.RemoteName, ul.RemoteProfile.Name = label, label, label
		ul.RemoteProfile.Username = meta.Username
		changed = true
	}
	iconKey := ""
	if team.LastTeamIconUpdate > 0 {
		iconKey = team.ID + "_" + strconv.FormatInt(team.LastTeamIconUpdate, 10)
	}
	if iconKey != meta.TeamIcon {
		mxc, err := c.uploadTeamIcon(team, iconKey)
		if err != nil {
			lg.Warn().Err(err).Str("team_id", team.ID).Msg("Could not set the team icon as the login avatar")
		} else {
			meta.TeamIcon = iconKey
			ul.RemoteProfile.Avatar = mxc
			c.setSpaceAvatar(mxc)
			changed = true
		}
	}
	if changed {
		c.setSpaceName(label)
		c.saveLogin()
	}
}

// uploadTeamIcon uploads the team icon, or returns "" for a team without one.
func (c *MattermostClient) uploadTeamIcon(team *mattermost.Team, iconKey string) (id.ContentURIString, error) {
	if iconKey == "" {
		return "", nil
	}
	if c.uploadTeamIconFn != nil {
		return c.uploadTeamIconFn(team)
	}
	ctx := context.Background()
	data, err := fetchImage(ctx, mattermost.TeamImageURL(c.serverURL(), team.ID), c.Token)
	if err != nil {
		return "", err
	}
	mxc, _, err := c.UserLogin.Bridge.Bot.UploadMedia(ctx, "", data, "team-icon", http.DetectContentType(data))
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	return mxc, nil
}

// setSpaceAvatar and setSpaceName update the login's own space, the one the
// client shows for the account. Best effort: the space may not exist.
func (c *MattermostClient) setSpaceAvatar(mxc id.ContentURIString) {
	c.setSpaceState(event.StateRoomAvatar, &event.RoomAvatarEventContent{URL: mxc})
}

func (c *MattermostClient) setSpaceName(label string) {
	name := label
	if c.UserLogin != nil && c.UserLogin.Bridge != nil && c.UserLogin.Bridge.Network != nil {
		name = fmt.Sprintf("%s (%s)", c.UserLogin.Bridge.Network.GetName().DisplayName, label)
	}
	c.setSpaceState(event.StateRoomName, &event.RoomNameEventContent{Name: name})
}

func (c *MattermostClient) setSpaceState(evtType event.Type, content any) {
	lg := c.lifecycleLogger()
	if c.setSpaceStateFn != nil {
		c.setSpaceStateFn(evtType, content)
		return
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.Bot == nil {
		return
	}
	ctx := context.Background()
	space, err := c.UserLogin.GetSpaceRoom(ctx)
	if err != nil || space == "" {
		return
	}
	if _, err := c.UserLogin.Bridge.Bot.SendState(ctx, space, evtType, "", &event.Content{Parsed: content}, time.Time{}); err != nil {
		lg.Warn().Err(err).Stringer("type", evtType).Msg("Could not update the login's space")
	}
}

func (c *MattermostClient) saveLogin() {
	lg := c.lifecycleLogger()
	if c.saveLoginFn != nil {
		c.saveLoginFn()
		return
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil {
		return
	}
	if err := c.UserLogin.Save(context.Background()); err != nil {
		lg.Err(err).Msg("Could not save the login")
	}
}

// removeForeignPortals deletes this login's rooms for channels another login
// bridges. Rooms that do not exist are skipped by the framework.
func (c *MattermostClient) removeForeignPortals(foreign map[string]bool) {
	lg := c.lifecycleLogger()
	if len(foreign) == 0 {
		return
	}
	c.mu.Lock()
	for id := range foreign {
		delete(c.channels, id)
	}
	c.mu.Unlock()
	for id := range foreign {
		if !c.hasPortal(id) {
			continue
		}
		lg.Info().Str("channel_id", id).Msg("Removing room: the channel belongs to another login")
		c.queueEvent(&mmChatDeleteEvent{
			portalKey: networkid.PortalKey{ID: networkid.PortalID(id), Receiver: c.UserLogin.ID},
		})
	}
}

// hasPortal reports whether this login has a room for the channel. Unknown
// (no database) counts as no: there is nothing to remove.
func (c *MattermostClient) hasPortal(channelID string) bool {
	if c.hasPortalFn != nil {
		return c.hasPortalFn(channelID)
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DB == nil {
		return false
	}
	p, err := c.UserLogin.Bridge.DB.Portal.GetByKey(context.Background(), networkid.PortalKey{ID: networkid.PortalID(channelID), Receiver: c.UserLogin.ID})
	return err == nil && p != nil && p.MXID != ""
}

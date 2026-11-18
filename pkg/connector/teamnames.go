package connector

import (
	"context"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Channel names carry their team when the account is in more than one team:
// "Town Square · Example". Each team is its own login (teamlogin.go), but a
// client may show every login with the same icon, and then the name is the
// only thing that tells two teams' Town Squares apart. DMs and group DMs
// have no team and keep their names.

const teamSuffixSep = " · "

// noteTeams records the account's teams for channel names.
func (c *MattermostClient) noteTeams(teams []*mattermost.Team) {
	labels := make(map[string]string, len(teams))
	for _, t := range teams {
		labels[t.ID] = teamLabel(t, teams)
	}
	c.mu.Lock()
	c.teamLabels = labels
	c.mu.Unlock()
}

// channelTitle is a channel's room name: its display name, with " · <team>"
// for a team channel when the account has more than one team.
func (c *MattermostClient) channelTitle(ch *mattermost.Channel) string {
	if ch == nil || ch.DisplayName == "" {
		return ""
	}
	if ch.TeamID == "" || ch.IsDM() || ch.IsGroupDM() {
		return ch.DisplayName
	}
	c.mu.RLock()
	label, multi := c.teamLabels[ch.TeamID], len(c.teamLabels) > 1
	c.mu.RUnlock()
	if !multi || label == "" {
		return ch.DisplayName
	}
	return ch.DisplayName + teamSuffixSep + label
}

// clearSpaceParents removes the room-side link (m.space.parent) to a team
// space from this login's rooms before the space is deleted. Leaving a space
// only removes the space-side link; a room still naming a deleted space as
// its parent is not counted as part of the login by clients that filter by
// account.
func (c *MattermostClient) clearSpaceParents(teamID string) {
	if c.clearSpaceParentsFn != nil {
		c.clearSpaceParentsFn(teamID)
		return
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DB == nil || c.UserLogin.Bridge.Bot == nil {
		return
	}
	ctx := context.Background()
	lg := c.lifecycleLogger()
	var spaceMXID id.RoomID
	for _, receiver := range []networkid.UserLoginID{c.UserLogin.ID, ""} {
		p, err := c.UserLogin.Bridge.DB.Portal.GetByKey(ctx, networkid.PortalKey{ID: teamPortalID(teamID), Receiver: receiver})
		if err == nil && p != nil && p.MXID != "" {
			spaceMXID = p.MXID
			break
		}
	}
	if spaceMXID == "" {
		return
	}
	portals, err := c.UserLogin.Bridge.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		lg.Warn().Err(err).Msg("Could not list rooms to unlink them from a team space")
		return
	}
	for _, p := range portals {
		if p.Receiver != c.UserLogin.ID || p.MXID == spaceMXID || isTeamPortal(p.ID) {
			continue
		}
		_, err := c.UserLogin.Bridge.Bot.SendState(ctx, p.MXID, event.StateSpaceParent, spaceMXID.String(),
			&event.Content{Parsed: &event.SpaceParentEventContent{}}, time.Time{})
		if err != nil {
			lg.Warn().Err(err).Stringer("room_id", p.MXID).Msg("Could not unlink room from team space")
		}
	}
}

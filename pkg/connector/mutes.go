package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Mutes, from Mattermost to Matrix only.
//
// Mattermost's "Mute channel" (notify_props.mark_unread = "mention") is
// copied to the Matrix room:
//   - a room created for a muted channel starts muted (ChatInfo.UserLocal);
//   - once per login, rooms that already existed are muted where the channel
//     is (never unmuted, so mutes made only in Matrix survive);
//   - muting or unmuting a channel in Mattermost does the same in Matrix.
//
// Mutes made in Matrix stay in Matrix: the connector deliberately does not
// implement bridgev2.MuteHandlingNetworkAPI, so muting a room never changes
// Mattermost's notifications. c.muted holds Mattermost's state as last seen,
// so a membership update that does not change it is not passed on.

// noteMemberships records Mattermost's mute state for the user's channels.
func (c *MattermostClient) noteMemberships(members []*mattermost.ChannelMember) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.muted == nil {
		c.muted = make(map[string]bool, len(members))
	}
	if c.lastViewed == nil {
		c.lastViewed = make(map[string]int64, len(members))
	}
	for _, m := range members {
		if m != nil && m.ChannelID != "" {
			c.muted[m.ChannelID] = m.Muted()
			c.lastViewed[m.ChannelID] = m.LastViewedAt
		}
	}
}

func (c *MattermostClient) channelMuted(channelID string) (muted, known bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	muted, known = c.muted[channelID]
	return
}

// swapMuted records muted for channelID and reports whether it changed.
func (c *MattermostClient) swapMuted(channelID string, muted bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.muted == nil {
		c.muted = make(map[string]bool)
	}
	old, known := c.muted[channelID]
	c.muted[channelID] = muted
	return !known || old != muted
}

// userLocalInfo is a room's mute: muted forever when the channel is muted in
// Mattermost, or when it is a team channel getting its room and
// network.mute_channels_on_create is on; otherwise nothing (the room keeps
// its own state). The create-time mute is never sent for an existing room, so
// unmuting a channel in Matrix sticks whatever the framework's mute settings.
func (c *MattermostClient) userLocalInfo(channelID string) *bridgev2.UserLocalPortalInfo {
	if muted, _ := c.channelMuted(channelID); muted {
		return &bridgev2.UserLocalPortalInfo{MutedUntil: &event.MutedForever}
	}
	if c.muteChannelsOnCreate() && c.isTeamChannel(channelID) && !c.hasPortal(channelID) {
		return &bridgev2.UserLocalPortalInfo{MutedUntil: &event.MutedForever}
	}
	return nil
}

// muteChannelsOnCreate reports network.mute_channels_on_create (off without
// a bridge config).
func (c *MattermostClient) muteChannelsOnCreate() bool {
	if c.muteChannelsFn != nil {
		return c.muteChannelsFn()
	}
	if cfg := bridgeConfig(c.bridge()); cfg != nil {
		return cfg.MuteChannelsOnCreate
	}
	return false
}

// isTeamChannel reports whether a known channel belongs to a team (not a DM
// or group DM).
func (c *MattermostClient) isTeamChannel(channelID string) bool {
	c.mu.RLock()
	ch := c.channels[channelID]
	c.mu.RUnlock()
	return ch != nil && ch.TeamID != "" && !ch.IsDM() && !ch.IsGroupDM()
}

func (c *MattermostClient) fetchMemberships(teamID string) ([]*mattermost.ChannelMember, error) {
	if c.membershipsFn != nil {
		return c.membershipsFn(teamID)
	}
	return mattermost.GetMyChannelMembers(c.serverURL(), c.Token, teamID)
}

// syncMemberships fetches a team's mute states. Failure leaves the previous
// state: mutes are a convenience and never block the channel sync.
func (c *MattermostClient) syncMemberships(teamID string) bool {
	members, err := c.fetchMemberships(teamID)
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Warn().Err(err).Str("team_id", teamID).Msg("Could not fetch channel mute states")
		return false
	}
	c.noteMemberships(members)
	return true
}

// muteMatrixRoom mutes (or unmutes) the room for a channel as the user. It
// reports false when the mute could not be applied; a channel without a room
// yet is not a failure (the room gets the mute when it is created).
func (c *MattermostClient) muteMatrixRoom(ctx context.Context, channelID string, muted bool) bool {
	until := bridgev2.Unmuted
	if muted {
		until = event.MutedForever
	}
	if c.muteRoomFn != nil {
		return c.muteRoomFn(channelID, until)
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.User == nil {
		return false
	}
	lg := c.lifecycleLogger()
	portal, err := c.UserLogin.Bridge.GetExistingPortalByKey(ctx, networkid.PortalKey{ID: networkid.PortalID(channelID), Receiver: c.UserLogin.ID})
	if err != nil {
		lg.Warn().Err(err).Str("channel_id", channelID).Msg("Could not look up the room to mirror a mute")
		return false
	}
	if portal == nil || portal.MXID == "" {
		return true
	}
	dp := c.UserLogin.User.DoublePuppet(ctx)
	if dp == nil {
		lg.Warn().Str("channel_id", channelID).Msg("No double puppet to mirror the Mattermost mute in Matrix")
		return false
	}
	if err := dp.MuteRoom(ctx, portal.MXID, until); err != nil {
		lg.Warn().Err(err).Str("channel_id", channelID).Msg("Could not mirror the Mattermost mute in Matrix")
		return false
	}
	return true
}

// syncExistingMutes mutes, once per login, the rooms that existed before
// mutes were bridged and whose channel is muted in Mattermost. complete is
// false when some team's mute states could not be fetched; the sync then
// waits for a later connect, as it does when a room could not be muted.
func (c *MattermostClient) syncExistingMutes(ctx context.Context, complete bool) {
	meta := c.loginMeta()
	if meta == nil || meta.MutesSynced || !complete {
		return
	}
	c.mu.RLock()
	var muted []string
	for id, m := range c.muted {
		if m {
			muted = append(muted, id)
		}
	}
	c.mu.RUnlock()
	for _, id := range muted {
		if c.ownsChannelID(id) && !c.muteMatrixRoom(ctx, id, true) {
			return
		}
	}
	meta.MutesSynced = true
	c.saveLogin()
}

// ownsChannelID reports whether this login bridges a channel it has seen.
func (c *MattermostClient) ownsChannelID(channelID string) bool {
	c.mu.RLock()
	ch := c.channels[channelID]
	c.mu.RUnlock()
	return ch != nil && c.ownsChannel(ch)
}

// handleMemberUpdated mirrors a mute change made in Mattermost.
func (c *MattermostClient) handleMemberUpdated(ctx context.Context, m *mattermost.ChannelMember) {
	if m == nil || (m.UserID != "" && m.UserID != c.UserID) {
		return
	}
	muted := m.Muted()
	if !c.swapMuted(m.ChannelID, muted) || !c.ownsChannelID(m.ChannelID) {
		return
	}
	c.muteMatrixRoom(ctx, m.ChannelID, muted)
}

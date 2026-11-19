package connector

import (
	"context"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Thread reads, Mattermost → Matrix.
//
// Matrix keeps a read receipt per thread, so marking a room read leaves its
// threads unread; clients that count followed threads then show threads the
// user has read in Mattermost as unread. Three paths send thread receipts:
//
//   - A thread read in Mattermost ("thread_read_changed").
//   - Viewing a channel marks the room's threads read up to the view, except
//     threads Mattermost counts as unread. With collapsed reply threads off
//     the replies were shown inline, so all of them are read; with them on,
//     Mattermost only counts followed threads as unread, so a thread it does
//     not count is not left unread in Matrix either.
//   - On connect, a catch-up: followed threads with no unread replies are
//     marked read up to when they were viewed, and each channel's threads as
//     for a view of the channel at its last view.
//
// A thread already marked read up to a point is not marked again, which also
// stops the receipt for a read made in Matrix coming back from Mattermost.

// threadLookback bounds the threads marked read for a channel view to those
// with messages in this period.
var threadLookback = 30 * 24 * time.Hour

const threadRootsQuery = `SELECT DISTINCT thread_root_id FROM message
WHERE bridge_id = $1 AND room_id = $2 AND room_receiver = $3
  AND thread_root_id IS NOT NULL AND thread_root_id <> '' AND timestamp >= $4`

// noteThreadRead records that a thread was read up to t and reports whether
// that is news.
func (c *MattermostClient) noteThreadRead(rootID string, t time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.threadReadAt == nil {
		c.threadReadAt = make(map[string]time.Time)
	}
	if prev, ok := c.threadReadAt[rootID]; ok && !t.After(prev) {
		return false
	}
	c.threadReadAt[rootID] = t
	return true
}

// queueThreadReceipt marks a thread read in Matrix, as the user.
func (c *MattermostClient) queueThreadReceipt(channelID, rootID string, upTo time.Time) {
	if rootID == "" || channelID == "" || upTo.IsZero() || !c.noteThreadRead(rootID, upTo) {
		return
	}
	c.queueEvent(&simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: networkid.PortalKey{ID: networkid.PortalID(channelID), Receiver: c.UserLogin.ID},
			Sender:    bridgev2.EventSender{IsFromMe: true, SenderLogin: c.UserLogin.ID},
			Timestamp: upTo,
		},
		ReadUpTo:   upTo,
		ThreadRoot: networkid.MessageID(rootID),
	})
}

// handleThreadRead mirrors a thread read in Mattermost.
func (c *MattermostClient) handleThreadRead(evt *mattermost.ThreadReadEvent) {
	if evt == nil || !c.bridgesChannel(evt.ChannelID) {
		return
	}
	t := c.now()
	if evt.Timestamp > 0 {
		t = time.UnixMilli(evt.Timestamp)
	}
	c.queueThreadReceipt(evt.ChannelID, evt.ThreadID, t)
}

// bridgesChannel reports whether this login has the channel.
func (c *MattermostClient) bridgesChannel(channelID string) bool {
	c.mu.RLock()
	ch := c.channels[channelID]
	c.mu.RUnlock()
	return ch != nil && c.ownsChannel(ch)
}

// readChannelThreads marks the room's threads read up to t, except, with
// collapsed threads on, those Mattermost counts as unread. When that count
// cannot be read, nothing is marked: threads then clear when read as threads.
func (c *MattermostClient) readChannelThreads(channelID string, t time.Time) {
	var unread map[string]bool
	if c.collapsedThreads() {
		var ok bool
		if unread, ok = c.unreadThreads(c.teamForChannel(channelID)); !ok {
			return
		}
	}
	for _, root := range c.threadRoots(channelID, t.Add(-threadLookback)) {
		if !unread[root] {
			c.queueThreadReceipt(channelID, root, t)
		}
	}
}

// unreadThreads is the set of threads Mattermost counts as unread in a team.
func (c *MattermostClient) unreadThreads(teamID string) (map[string]bool, bool) {
	if teamID == "" {
		return nil, false
	}
	var ids []string
	var err error
	if c.unreadThreadsFn != nil {
		ids, err = c.unreadThreadsFn(teamID)
	} else {
		ids, err = mattermost.GetUnreadThreadIDs(c.serverURL(), c.Token, teamID)
	}
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Warn().Err(err).Str("team_id", teamID).Msg("Could not list unread threads")
		return nil, false
	}
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, true
}

// threadRoots lists the roots of the room's threads with messages since.
func (c *MattermostClient) threadRoots(channelID string, since time.Time) []string {
	if c.threadRootsFn != nil {
		return c.threadRootsFn(channelID, since)
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DB == nil {
		return nil
	}
	ctx := context.Background()
	rows, err := c.UserLogin.Bridge.DB.Query(ctx, threadRootsQuery, c.UserLogin.Bridge.ID, channelID, c.UserLogin.ID, since.UnixNano())
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Warn().Err(err).Str("channel_id", channelID).Msg("Could not list the room's threads")
		return nil
	}
	defer rows.Close()
	var roots []string
	for rows.Next() {
		var root string
		if rows.Scan(&root) == nil && root != "" {
			roots = append(roots, root)
		}
	}
	return roots
}

// collapsedThreads reports whether the user sees collapsed reply threads,
// asked once per connect (resetCollapsedThreads). An unanswered question
// counts as on: threads then clear only when read as threads, as before.
func (c *MattermostClient) collapsedThreads() bool {
	if c.collapsedThreadsFn != nil {
		return c.collapsedThreadsFn()
	}
	c.mu.RLock()
	on, known := c.collapsedOn, c.collapsedKnown
	c.mu.RUnlock()
	if known {
		return on
	}
	on, err := mattermost.CollapsedThreads(c.serverURL(), c.Token)
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Warn().Err(err).Msg("Could not read the collapsed threads setting")
		return true
	}
	c.mu.Lock()
	c.collapsedOn, c.collapsedKnown = on, true
	c.mu.Unlock()
	return on
}

func (c *MattermostClient) resetCollapsedThreads() {
	c.mu.Lock()
	c.collapsedKnown = false
	c.mu.Unlock()
}

func (c *MattermostClient) userThreads(teamID string) ([]*mattermost.UserThread, error) {
	if c.userThreadsFn != nil {
		return c.userThreadsFn(teamID)
	}
	return mattermost.GetUserThreads(c.serverURL(), c.Token, teamID)
}

// catchUpThreadReads marks threads read in Matrix that Mattermost has as
// read, for the teams synced on connect.
func (c *MattermostClient) catchUpThreadReads(teams []*mattermost.Team) {
	c.resetCollapsedThreads()
	for _, team := range teams {
		threads, err := c.userThreads(team.ID)
		if err != nil {
			lg := c.lifecycleLogger()
			lg.Warn().Err(err).Str("team_id", team.ID).Msg("Could not list followed threads")
			continue
		}
		for _, th := range threads {
			if th == nil || th.UnreadReplies > 0 || th.LastViewedAt <= 0 || !c.bridgesChannel(th.Post.ChannelID) {
				continue
			}
			c.queueThreadReceipt(th.Post.ChannelID, th.ID, time.UnixMilli(th.LastViewedAt))
		}
	}
	for channelID, viewed := range c.channelViews() {
		if viewed > 0 && c.bridgesChannel(channelID) {
			c.readChannelThreads(channelID, time.UnixMilli(viewed))
		}
	}
}

// channelViews is when each channel was last viewed, from the memberships
// fetched on connect.
func (c *MattermostClient) channelViews() map[string]int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]int64, len(c.lastViewed))
	for id, t := range c.lastViewed {
		out[id] = t
	}
	return out
}

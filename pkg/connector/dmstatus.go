package connector

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// DM status: the other person's Mattermost status, shown on a 1:1 DM's room
// title, with their custom status text as the room topic.
//
// Beeper's homeserver drops Matrix presence, so the title is the one place a
// status is always visible (chat list and header). It is DM-only: a channel's
// single title cannot carry each member's status. Title and topic changes are
// excluded from the timeline.
//
//	all  (default) "alice 🟢", "alice 🟡", "alice ⛔", "alice ⚪"
//	dnd            "alice" normally, "alice ⛔" while Do Not Disturb is on
//	off            "alice", empty topic
//
// A custom status emoji follows the state ("alice 🟢 🏖️"); its text becomes the
// topic. Server custom emoji (uploaded images) cannot appear in a text title
// and are left out.
//
// One function (dmRoomInfo) decides the title and topic. Room syncs and live
// updates both use it, so they cannot disagree, and every status poll re-applies
// it, which also clears expired custom statuses and repairs missed updates.

// Values of network.dm_status.
const (
	dmStatusAll = "all"
	dmStatusDND = "dnd"
	dmStatusOff = "off"
)

// statusEmoji is the title marker for each Mattermost status.
var statusEmoji = map[string]string{
	"online":  "🟢",
	"away":    "🟡",
	"dnd":     "⛔",
	"offline": "⚪",
}

// parseDMStatus validates network.dm_status; empty means all.
func parseDMStatus(setting string) (string, error) {
	switch setting {
	case "", dmStatusAll:
		return dmStatusAll, nil
	case dmStatusDND, dmStatusOff:
		return setting, nil
	}
	return "", fmt.Errorf("unknown value %q (want all, dnd or off)", setting)
}

// dmStatusMode returns the configured mode. Start rejects invalid values, so
// an invalid one here can only come from a direct caller; it is treated as off.
func (c *MattermostClient) dmStatusMode() string {
	setting := ""
	if c.dmStatusFn != nil {
		setting = c.dmStatusFn()
	} else if c.UserLogin != nil && c.UserLogin.Bridge != nil {
		if conn, ok := c.UserLogin.Bridge.Network.(*MattermostConnector); ok && conn.Config != nil {
			setting = conn.Config.DMStatus
		}
	}
	mode, err := parseDMStatus(setting)
	if err != nil {
		return dmStatusOff
	}
	return mode
}

// dmTitle builds a DM title from the person's name, their Mattermost status
// and their custom status. An unknown status (not polled yet) adds no marker.
func dmTitle(name, status string, custom *mattermost.CustomStatus, mode string) string {
	if mode == dmStatusOff {
		return name
	}
	parts := []string{name}
	if mode == dmStatusAll || status == "dnd" {
		if e := statusEmoji[status]; e != "" {
			parts = append(parts, e)
		}
	}
	if custom != nil {
		// Unicode emoji only: a name with no Unicode form is a server custom
		// emoji, which a text title cannot show.
		if e := mmEmojiToUnicode(custom.Emoji); e != custom.Emoji {
			parts = append(parts, strings.TrimSpace(e))
		}
	}
	return strings.Join(parts, " ")
}

// dmRoomInfo returns the title and topic for a 1:1 DM room, or nils when the
// other person's profile is not known yet (leave the room alone rather than
// blank it). Mattermost DMs have no topic of their own, so the bridge owns it:
// the custom status text, or empty.
func (c *MattermostClient) dmRoomInfo(ch *mattermost.Channel) (title, topic *string) {
	if ch == nil || !ch.IsDM() {
		return nil, nil
	}
	otherID := dmOtherUserID(ch.Name, c.UserID)
	if otherID == "" {
		return nil, nil
	}
	c.mu.RLock()
	u := c.users[otherID]
	status := c.presenceSeen[otherID]
	c.mu.RUnlock()
	if u == nil {
		return nil, nil
	}
	if u.IsBot {
		status = "" // Mattermost shows bots with a BOT tag, not a status
	}
	mode := c.dmStatusMode()
	var custom *mattermost.CustomStatus
	if mode != dmStatusOff {
		custom = u.ActiveCustomStatus(c.now())
	}
	t := dmTitle(c.displayName(u), status, custom, mode)
	text := ""
	if custom != nil {
		text = custom.Text
	}
	return &t, &text
}

func (c *MattermostClient) now() time.Time {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Now()
}

// dmStatusState remembers what was last queued per DM, so an unchanged title
// is not re-queued on every poll.
type dmStatusState struct {
	mu   sync.Mutex
	sent map[string]string // channel ID -> title + "\x00" + topic
}

// noteDMRoomInfo records the title and topic that were just queued for a DM
// and reports whether they differ from the previous ones.
func (c *MattermostClient) noteDMRoomInfo(channelID string, title, topic *string) bool {
	key := ""
	if title != nil {
		key = *title
	}
	key += "\x00"
	if topic != nil {
		key += *topic
	}
	s := &c.dmStatus
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent == nil {
		s.sent = make(map[string]string)
	}
	if s.sent[channelID] == key {
		return false
	}
	s.sent[channelID] = key
	return true
}

// dmStatusEvents builds the room updates for every 1:1 DM whose title or topic
// changed since it was last queued.
func (c *MattermostClient) dmStatusEvents() []*simplevent.ChatInfoChange {
	c.mu.RLock()
	var dms []*mattermost.Channel
	for _, ch := range c.channels {
		if ch != nil && ch.IsDM() && ch.DeleteAt == 0 {
			dms = append(dms, ch)
		}
	}
	c.mu.RUnlock()
	var out []*simplevent.ChatInfoChange
	for _, ch := range dms {
		title, topic := c.dmRoomInfo(ch)
		if title == nil || !c.noteDMRoomInfo(ch.ID, title, topic) {
			continue
		}
		channelID, logTitle := ch.ID, *title
		out = append(out, &simplevent.ChatInfoChange{
			EventMeta: simplevent.EventMeta{
				Type:      bridgev2.RemoteEventChatInfoChange,
				PortalKey: networkid.PortalKey{ID: networkid.PortalID(channelID), Receiver: c.loginID()},
				LogContext: func(z zerolog.Context) zerolog.Context {
					return z.Str("source", "dm_status").Str("title", logTitle)
				},
			},
			ChatInfoChange: &bridgev2.ChatInfoChange{
				ChatInfo: &bridgev2.ChatInfo{
					Name:                       title,
					Topic:                      topic,
					ExcludeChangesFromTimeline: true,
				},
			},
		})
	}
	return out
}

// refreshDMStatuses queues the changed DM titles and topics.
func (c *MattermostClient) refreshDMStatuses() {
	if c.dmStatusMode() == dmStatusOff {
		return // titles are plain; room syncs keep them that way
	}
	for _, ev := range c.dmStatusEvents() {
		c.queueEvent(ev)
	}
}

func (c *MattermostClient) loginID() networkid.UserLoginID {
	if c.UserLogin == nil {
		return ""
	}
	return c.UserLogin.ID
}

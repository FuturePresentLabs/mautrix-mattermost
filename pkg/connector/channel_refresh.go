package connector

import (
	"context"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// mmChannelRefreshEvent is the placeholder queued to bring a room's power
// levels up to date with its channel's lifecycle.
//
// Power levels ride on chat info now, so the refresh has to fetch the channel
// and queue the resync that carries the overrides. Doing that here rather than
// at the call site keeps REST off the websocket loop, and running in PreHandle
// puts the update ahead of any notice queued behind it — a restored channel
// must be writable before its "unarchived" post is relayed.
//
// Like the framework's other placeholders its type is RemoteEventUnknown:
// bridgev2 calls PreHandle on the portal lane and then ignores the event.
type mmChannelRefreshEvent struct {
	client    *MattermostClient
	portalKey networkid.PortalKey
	channelID string
}

func (e *mmChannelRefreshEvent) GetType() bridgev2.RemoteEventType {
	return bridgev2.RemoteEventUnknown
}
func (e *mmChannelRefreshEvent) GetPortalKey() networkid.PortalKey { return e.portalKey }
func (e *mmChannelRefreshEvent) AddLogContext(c zerolog.Context) zerolog.Context {
	return c.Str("channel_id", e.channelID).Str("lifecycle", "power-refresh")
}
func (e *mmChannelRefreshEvent) GetSender() bridgev2.EventSender { return bridgev2.EventSender{} }

// PreHandle runs on the portal lane when the event is dequeued.
func (e *mmChannelRefreshEvent) PreHandle(ctx context.Context, portal *bridgev2.Portal) {
	if e == nil || e.client == nil || portal == nil {
		return
	}
	ch, err := mattermost.GetChannelContext(ctx, e.client.serverURL(), e.client.Token, e.channelID)
	if err != nil || ch == nil || ch.ID != e.channelID {
		// Fail closed: a room whose channel cannot be read keeps the power
		// levels it already has, rather than being made writable on a guess.
		zerolog.Ctx(ctx).Debug().Err(err).
			Str("channel_id", e.channelID).
			Msg("Could not read the channel to refresh its power levels")
		return
	}
	e.client.noteChannelAlive(e.channelID)
	e.client.queueChannelResync(ch)
}

// queueChannelRefresh queues a power-level refresh for the channel. It performs
// no I/O itself.
func (c *MattermostClient) queueChannelRefresh(channelID string) {
	if channelID == "" {
		return
	}
	c.queueEvent(&mmChannelRefreshEvent{
		client:    c,
		portalKey: networkid.PortalKey{ID: networkid.PortalID(channelID), Receiver: c.receiverID()},
		channelID: channelID,
	})
}

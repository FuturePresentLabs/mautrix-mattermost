package connector

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
)

var (
	// errNilRemoteEvent is returned when a caller tries to queue a nil event.
	errNilRemoteEvent = errors.New("cannot queue a nil remote event")
	// errNoEventQueue is returned when the client has no user login to deliver
	// through, so the event would otherwise be dropped silently.
	errNoEventQueue = errors.New("no event queue attached")
)

// queueEvent delivers a remote event through the login's queue. Kept here
// because several fork features (acknowledgements, burn-on-read, the channel
// lifecycle) need to queue events and a test seam.
func (c *MattermostClient) queueEvent(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
	if ev == nil {
		return bridgev2.EventHandlingResultFailed.WithError(errNilRemoteEvent)
	}
	if c.queueRemoteEventFn != nil {
		return c.queueRemoteEventFn(ev)
	}
	if c.UserLogin == nil {
		return bridgev2.EventHandlingResultFailed.WithError(errNoEventQueue)
	}
	return c.UserLogin.QueueRemoteEvent(ev)
}

func (c *MattermostClient) lifecycleLogger() zerolog.Logger {
	if c.UserLogin != nil {
		return c.UserLogin.Log
	}
	return zerolog.Nop()
}

// receiverID is the login a portal belongs to. Split portals key their spaces on
// it, so it must match what the framework uses for channel parents.
func (c *MattermostClient) receiverID() networkid.UserLoginID {
	if c.UserLogin == nil || c.UserLogin.UserLogin == nil {
		return ""
	}
	return c.UserLogin.ID
}

// deferredDeletionSupported reports whether portal events are handled on a lane
// that preserves order. Features that rely on a restore being applied after the
// check it invalidates (acknowledgements, burn-on-read) must not act when the
// framework is free to run them concurrently.
func (c *MattermostClient) deferredDeletionSupported() (bool, string) {
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.Config == nil {
		// No bridge means no portal lane and no configuration we can reason
		// about; never treat that as permission to delete.
		return false, "bridge-config-unavailable"
	}
	if c.UserLogin.Bridge.Config.AsyncEvents {
		// Each portal event is handled in its own goroutine, so a restore
		// queued after this check can be handled before it (or concurrently).
		return false, "async-events-unsupported"
	}
	if c.UserLogin.Bridge.Config.PortalEventBuffer == 0 {
		// Events are handled inline in whatever goroutine queued them, which
		// removes the lane boundary this design relies on.
		return false, "portal-event-buffer-inline"
	}
	return true, ""
}

// startLifecyclePoll runs the channel lifecycle poll for as long as the client
// is connected. The interval is configuration: with none set, lifecycle state
// is left to the websocket events alone.
func (c *MattermostClient) startLifecyclePoll() {
	interval, err := bridgeConfig(c.bridge()).channelPollInterval()
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Warn().Err(err).Msg("Ignoring an invalid channel lifecycle poll interval")
		return
	}
	if interval <= 0 || c.done == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-c.done
		cancel()
	}()
	c.runLifecyclePoll(ctx, interval)
}

// mmLifecycleHintEvent carries a "look at this channel" hint onto the portal
// lane.
//
// The probe itself is deliberately not done in the websocket loop, and the
// decision is deliberately made at dequeue: a hint queued before a restore is
// harmless because the probe re-reads the channel before anything is deleted.
type mmLifecycleHintEvent struct {
	client      *MattermostClient
	portalKey   networkid.PortalKey
	channelID   string
	allowDelete bool
}

func (e *mmLifecycleHintEvent) GetType() bridgev2.RemoteEventType {
	return bridgev2.RemoteEventUnknown
}
func (e *mmLifecycleHintEvent) GetPortalKey() networkid.PortalKey { return e.portalKey }
func (e *mmLifecycleHintEvent) AddLogContext(c zerolog.Context) zerolog.Context {
	return c.Str("channel_id", e.channelID).Str("lifecycle", "hint")
}
func (e *mmLifecycleHintEvent) GetSender() bridgev2.EventSender { return bridgev2.EventSender{} }

// PreHandle runs on the portal lane when the event is dequeued.
func (e *mmLifecycleHintEvent) PreHandle(ctx context.Context, portal *bridgev2.Portal) {
	if e == nil || e.client == nil || portal == nil {
		return
	}
	e.client.handleChannelLifecycle(ctx, e.channelID, e.allowDelete, e.client.queueLifecycleEvent)
}

// queueLifecycleCheck asks for a channel's lifecycle to be verified. allowDelete
// is false when the hint cannot prove a deletion on its own: a restore path
// must never delete a room it was only told to refresh.
func (c *MattermostClient) queueLifecycleCheck(channelID string, allowDelete bool) {
	if channelID == "" {
		return
	}
	// Hints reach every login of the account. Asking about a channel this login
	// never bridged materialises a portal row for it: the framework inserts the
	// portal for the event's key before any handler can decide it is not ours.
	// Without a room there is nothing to refresh or delete anyway.
	if !c.hasPortal(channelID) {
		return
	}
	c.queueEvent(&mmLifecycleHintEvent{
		client:      c,
		portalKey:   networkid.PortalKey{ID: networkid.PortalID(channelID), Receiver: c.receiverID()},
		channelID:   channelID,
		allowDelete: allowDelete,
	})
}

// reconnectWebsocket replaces the websocket so its membership snapshot is
// rebuilt. Mattermost caches that snapshot per connection with archived
// channels excluded, so a connection that missed an archive or a restore keeps
// missing that channel's events until it is replaced.
//
// The close is marked quiet, so asking for it is not reported as a disconnect
// (see disconnectState).
func (c *MattermostClient) reconnectWebsocket() {
	if c.reconnectFn != nil {
		c.reconnectFn()
		return
	}
	c.mu.RLock()
	cl := c.client
	c.mu.RUnlock()
	if cl == nil {
		return
	}
	c.quietReconnect.Store(true)
	cl.Close()
}

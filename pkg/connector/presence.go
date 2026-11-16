package connector

import (
	"context"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Why this file exists: Mattermost already tells us who is around. Every connected
// websocket client receives a status_change event whenever a user in the workspace
// goes online, away, into Do Not Disturb or offline, and pkg/mattermost turns those
// into StatusChangeEvent. Matrix has no presence in the Desktop API weeper talks to
// and no bridge-side presence ABI either, so the cheapest way to carry the status
// out is the one mautrix-go already implements: Matrix presence on the bridged
// ghost's own MXID.
//
// This is opt-in (network.presence.enabled) for two reasons. It costs one homeserver
// request per status change, and a homeserver that does not store presence gains
// nothing but the traffic. Note that presence is only visible on homeservers that
// implement it: Beeper's hungryserv serves a narrow Matrix subset and drops it, so a
// Beeper-hosted deployment simply gets no presence, without errors.
//
// What is deliberately not published:
//   - Only users the bridge has already created a ghost for. A ghost that shares no
//     room has nobody to show presence to, and resolving status changes for the whole
//     workspace must not create ghost rows.
//   - The logged-in user's own status. Their MXID belongs to a real Matrix account
//     whose presence is set by their own clients; the bridge must not fight them.

// PresenceConfig is the network.presence block of the bridge configuration.
type PresenceConfig struct {
	// Enabled republishes Mattermost user statuses as Matrix presence for the
	// bridged ghost. Off by default.
	Enabled bool `yaml:"enabled"`
}

const (
	// presenceQueueSize bounds how many status changes may wait for the publisher.
	// Status churn is low-rate; the buffer only exists so the websocket loop never
	// blocks on a slow homeserver.
	presenceQueueSize = 32
	// presenceTimeout bounds one presence request. A homeserver that is slow to
	// answer must not keep a worker (or a context) alive indefinitely.
	presenceTimeout = 15 * time.Second
)

// presenceUpdate is one status change on its way to the homeserver.
type presenceUpdate struct {
	userID string
	status string
}

// presenceSetter publishes one presence state for a ghost. It is the shape of
// (*appservice.AppService).Intent(mxid).SetPresence, kept as a function so the
// mapping can be exercised without a homeserver.
type presenceSetter func(ctx context.Context, mxid id.UserID, req mautrix.ReqPresence) error

// ghostResolver returns the MXID of an existing ghost, and false when the user has
// no ghost yet (nobody bridged a message from them).
type ghostResolver func(ctx context.Context, userID string) (id.UserID, bool, error)

// matrixPresence maps a Mattermost status to the Matrix presence to publish.
// Mattermost's Do Not Disturb has no Matrix equivalent, so it rides on the status
// message of an otherwise online presence. Empty or unknown statuses say nothing
// about the user and are not published.
func matrixPresence(mmStatus string) (mautrix.ReqPresence, bool) {
	switch mmStatus {
	case "online":
		return mautrix.ReqPresence{Presence: "online"}, true
	case "away":
		return mautrix.ReqPresence{Presence: "unavailable"}, true
	case "dnd":
		return mautrix.ReqPresence{Presence: "online", StatusMsg: "Do Not Disturb"}, true
	case "offline":
		return mautrix.ReqPresence{Presence: "offline"}, true
	default:
		return mautrix.ReqPresence{}, false
	}
}

// publishPresence resolves the sender's ghost and publishes the mapped presence.
// A user with no ghost, and a status with no mapping, are both normal no-ops: the
// caller logs only real failures.
func publishPresence(ctx context.Context, set presenceSetter, resolve ghostResolver, update presenceUpdate) error {
	req, ok := matrixPresence(update.status)
	if !ok {
		return nil
	}
	mxid, ok, err := resolve(ctx, update.userID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return set(ctx, mxid, req)
}

// startPresencePublisher starts the worker that republishes status changes. The
// work is detached from the websocket loop: a homeserver answering slowly must not
// delay message delivery. The worker exits with the connection it was started for.
func (c *MattermostClient) startPresencePublisher(done <-chan struct{}) {
	updates := make(chan presenceUpdate, presenceQueueSize)
	c.mu.Lock()
	c.presenceUpdates = updates
	c.mu.Unlock()
	go func() {
		for {
			select {
			case <-done:
				return
			case update := <-updates:
				c.publishPresence(update)
			}
		}
	}()
}

// queuePresenceUpdate hands a status change to the worker without blocking. When
// the queue is full the update is already stale, so the newest one is dropped
// rather than delaying the websocket loop.
func (c *MattermostClient) queuePresenceUpdate(update presenceUpdate) {
	c.mu.RLock()
	updates := c.presenceUpdates
	c.mu.RUnlock()
	if updates == nil {
		return
	}
	select {
	case updates <- update:
	default:
	}
}

// publishPresence sends one status change, if presence is enabled and this build can
// reach the appservice.
func (c *MattermostClient) publishPresence(update presenceUpdate) {
	set, ok := c.presenceSetter()
	if !ok {
		return
	}
	resolve := func(ctx context.Context, userID string) (id.UserID, bool, error) {
		br := c.UserLogin.Bridge
		ghost, err := br.GetExistingGhostByID(ctx, networkid.UserID(userID))
		if err != nil || ghost == nil {
			return "", false, err
		}
		intent := br.Matrix.GhostIntent(ghost.ID)
		if intent == nil {
			return "", false, nil
		}
		mxid := intent.GetMXID()
		return mxid, mxid != "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), presenceTimeout)
	defer cancel()
	if err := publishPresence(ctx, set, resolve, update); err != nil {
		lg := c.lifecycleLogger()
		lg.Debug().Err(err).
			Str("mm_user_id", update.userID).
			Str("mm_status", update.status).
			Msg("Failed to publish Mattermost status as Matrix presence")
	}
}

// presenceSetter returns the appservice-backed setter, or false when presence is
// disabled by configuration or the Matrix connector is not the appservice one.
func (c *MattermostClient) presenceSetter() (presenceSetter, bool) {
	conn, ok := c.UserLogin.Bridge.Network.(*MattermostConnector)
	if !ok || conn.Config == nil || !conn.Config.Presence.Enabled {
		return nil, false
	}
	mx, ok := c.UserLogin.Bridge.Matrix.(*matrix.Connector)
	if !ok || mx.AS == nil {
		return nil, false
	}
	return func(ctx context.Context, mxid id.UserID, req mautrix.ReqPresence) error {
		return mx.AS.Intent(mxid).SetPresence(ctx, req)
	}, true
}

// handleStatusChange is the websocket entry point for a status_change event.
func (c *MattermostClient) handleStatusChange(evt *mattermost.StatusChangeEvent) {
	if evt == nil || evt.UserID == "" {
		return
	}
	if c.UserID != "" && evt.UserID == c.UserID {
		return // the logged-in user's presence belongs to their own clients
	}
	c.queuePresenceUpdate(presenceUpdate{userID: evt.UserID, status: evt.Status})
}

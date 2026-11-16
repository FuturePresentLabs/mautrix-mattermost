package connector

import (
	"context"
	"sort"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Why this file exists: Mattermost knows who is around, and the bridge can hand that
// to Matrix as presence on each bridged ghost's MXID, through the one presence API
// mautrix-go already implements.
//
// The status does not arrive over the websocket. Mattermost addresses its
// status_change event at the affected user's own connections (NewWebSocketEvent with
// the user id set), so a bridge connected as one user only ever hears about that user.
// Presence therefore comes from POST /api/v4/users/status/ids, polled for the users the
// bridge knows. A status_change frame is still useful as a hint that wakes the poller
// early.
//
// This is opt-in (network.presence.enabled) for two reasons: it costs a handful of
// Mattermost requests per interval, and a homeserver that does not store presence gains
// nothing but the traffic. Presence is only visible on homeservers that implement it:
// Beeper's hungryserv serves a narrow Matrix subset and drops it, so a Beeper-hosted
// deployment simply gets no presence, without errors.
//
// What is deliberately left alone:
//   - Users with no ghost. Nothing would see their presence, and polling the whole
//     workspace must not invent Matrix users.
//   - The logged-in user's own status. Their MXID belongs to a real Matrix account
//     whose presence is set by their own clients; the bridge must not fight them.

// PresenceConfig is the network.presence block of the bridge configuration.
type PresenceConfig struct {
	// Enabled republishes Mattermost user statuses as Matrix presence for the
	// bridged ghost. Off by default.
	Enabled bool `yaml:"enabled"`
}

const (
	// presencePollInterval is how often statuses are read. Presence is ambient, so
	// half a minute of staleness is not worth more traffic.
	presencePollInterval = 30 * time.Second
	// presenceBatchSize is how many user ids go into one status request.
	presenceBatchSize = 100
	// presenceTimeout bounds one presence request. A homeserver that is slow to
	// answer must not keep the poller blocked indefinitely.
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

// startPresencePoller starts the loop that keeps Matrix presence in step with
// Mattermost. It runs only when presence is enabled: a disabled bridge must not spend
// requests finding out.
func (c *MattermostClient) startPresencePoller(done <-chan struct{}) {
	if _, ok := c.presenceSetter(); !ok {
		lg := c.lifecycleLogger()
		conn, isConnector := c.UserLogin.Bridge.Network.(*MattermostConnector)
		mx, isMatrixConnector := c.UserLogin.Bridge.Matrix.(*matrix.Connector)
		lg.Debug().
			Bool("network_is_connector", isConnector).
			Bool("config_present", isConnector && conn.Config != nil).
			Bool("enabled", isConnector && conn.Config != nil && conn.Config.Presence.Enabled).
			Bool("matrix_is_connector", isMatrixConnector).
			Bool("appservice_present", isMatrixConnector && mx.AS != nil).
			Msg("Presence publishing is off")
		return
	}
	lg := c.lifecycleLogger()
	lg.Debug().Dur("interval", presencePollInterval).Msg("Starting presence poller")
	wake := make(chan struct{}, 1)
	c.mu.Lock()
	c.presenceWake = wake
	c.presenceSeen = map[string]string{}
	c.mu.Unlock()
	go func() {
		timer := time.NewTimer(presencePollInterval)
		defer timer.Stop()
		// The first pass publishes everyone's current status instead of waiting a
		// full interval for it.
		c.pollPresence()
		for {
			select {
			case <-done:
				return
			case <-wake:
			case <-timer.C:
			}
			c.pollPresence()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(presencePollInterval)
		}
	}()
}

// pollPresence reads the status of every user the bridge knows and publishes the ones
// that changed. A read failure publishes nothing: a partial answer is not evidence
// that anyone went offline.
func (c *MattermostClient) pollPresence() {
	if _, ok := c.presenceSetter(); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), presenceTimeout)
	defer cancel()
	ids := c.presenceCandidates(ctx)
	if len(ids) == 0 {
		lg := c.lifecycleLogger()
		lg.Debug().Msg("Presence poll found no mattermost users to ask about")
		return
	}
	statuses := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += presenceBatchSize {
		end := min(start+presenceBatchSize, len(ids))
		batch, err := mattermost.GetStatusesByIDs(c.serverURL(), c.Token, ids[start:end])
		if err != nil {
			lg := c.lifecycleLogger()
			lg.Debug().Err(err).Msg("Failed to read Mattermost statuses")
			return
		}
		for userID, status := range batch {
			statuses[userID] = status
		}
	}
	c.mu.Lock()
	previous := c.presenceSeen
	c.presenceSeen = statuses
	c.mu.Unlock()
	for _, update := range presenceDiff(previous, statuses) {
		c.publishPresence(update)
	}
	lg := c.lifecycleLogger()
	lg.Debug().
		Int("candidates", len(ids)).
		Int("changes", len(presenceDiff(previous, statuses))).
		Msg("Polled Mattermost statuses")
}

// presenceGhostQuery lists the Mattermost users the bridge has a ghost for. Only
// ghosts get presence: a Matrix user nobody shares a room with has nobody to show a
// status to, and polling must not invent users either.
const presenceGhostQuery = "SELECT id FROM ghost WHERE bridge_id = $1"

// presenceCandidates lists the Mattermost users worth asking about. Ghosts are read
// from the database rather than from the in-memory user cache, so a restart does not
// blind the poller until someone happens to post.
func (c *MattermostClient) presenceCandidates(ctx context.Context) []string {
	rows, err := c.UserLogin.Bridge.DB.Query(ctx, presenceGhostQuery, c.UserLogin.Bridge.ID)
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Debug().Err(err).Msg("Failed to list ghosts for presence")
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			lg := c.lifecycleLogger()
			lg.Debug().Err(err).Msg("Failed to read ghost id for presence")
			return nil
		}
		ids = append(ids, userID)
	}
	return filterPresenceIDs(ids, c.UserID)
}

// filterPresenceIDs drops empty ids and the logged-in user, whose MXID is a real
// Matrix account whose presence their own clients own. Sorted, so a poll is
// reproducible.
func filterPresenceIDs(ids []string, selfUserID string) []string {
	out := make([]string, 0, len(ids))
	for _, userID := range ids {
		if userID == "" || (selfUserID != "" && userID == selfUserID) {
			continue
		}
		out = append(out, userID)
	}
	sort.Strings(out)
	return out
}

// presenceDiff returns the statuses that differ from the previous poll. Users who
// disappear from the answer are left alone: silence is not the same as going offline.
func presenceDiff(previous, next map[string]string) []presenceUpdate {
	updates := make([]presenceUpdate, 0, len(next))
	for userID, status := range next {
		if previous[userID] == status {
			continue
		}
		updates = append(updates, presenceUpdate{userID: userID, status: status})
	}
	sort.Slice(updates, func(i, j int) bool { return updates[i].userID < updates[j].userID })
	return updates
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

// handleStatusChange is the websocket entry point for a status_change event. It is
// only a hint: Mattermost addresses that event at the affected user's own
// connections, so the bridge mostly sees its own login's status. Waking the poller
// just refreshes presence sooner than the next tick.
func (c *MattermostClient) handleStatusChange(*mattermost.StatusChangeEvent) {
	c.mu.RLock()
	wake := c.presenceWake
	c.mu.RUnlock()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

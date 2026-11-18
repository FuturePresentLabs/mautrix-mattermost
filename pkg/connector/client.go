package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// MattermostClient implements bridgev2.NetworkAPI for a single Mattermost user login.
type MattermostClient struct {
	UserLogin *bridgev2.UserLogin
	Username  string
	Token     string
	ServerURL string
	UserID    string // Mattermost user ID of the logged-in user

	// baseURL may be overridden in tests to point at a mock server.
	baseURL string

	client   *mattermost.Client
	mu       sync.RWMutex
	channels map[string]*mattermost.Channel // keyed by channel ID
	users    map[string]*mattermost.User    // keyed by user ID
	// mentions caches username lookups for @mentions (see mentions.go).
	mentions mentionCache
	// ghostMXIDFn and parseGhostFn override the ghost <-> Matrix ID mapping in
	// tests. When nil, the bridge's Matrix connector is used.
	ghostMXIDFn  func(mmUserID string) id.UserID
	parseGhostFn func(mxid id.UserID) string
	// ghostNameFn overrides reading a ghost's Matrix name in tests.
	ghostNameFn func(mmUserID string) string
	// usersByIDsFn and refreshGhostFn override profile fetches and ghost
	// updates in tests.
	usersByIDsFn   func(ids []string) ([]*mattermost.User, error)
	refreshGhostFn func(u *mattermost.User)
	// nameFormat is the Mattermost name format in use (see displaynames.go).
	// Guarded by mu.
	nameFormat string
	// displayNamesFn overrides network.display_names in tests.
	// dmStatus remembers the DM titles last queued (see dmstatus.go).
	dmStatus dmStatusState
	// dmStatusFn and nowFn override network.dm_status and the clock in tests.
	dmStatusFn     func() string
	nowFn          func() time.Time
	displayNamesFn func() string
	// channelMembersFn overrides the channel member fetch in tests.
	channelMembersFn func(channelID string) ([]*mattermost.ChannelMember, error)
	// presenceWake wakes the presence poller early (see presence.go). Guarded by mu
	// because Connect installs it and the websocket loop may already be running.
	presenceWake chan struct{}
	// presenceSeen is the status last published per Mattermost user id, so a poll
	// only publishes changes. Guarded by mu.
	presenceSeen map[string]string
	done         chan struct{} // closed by Disconnect to stop reconnect loop

	// probeChannelFn overrides the channel lifecycle probe in tests. When nil,
	// mattermost.ProbeChannel is used.
	probeChannelFn func(serverURL, token, channelID string) mattermost.ChannelProbe
	// listChannelStatesFn and reconnectFn override the lifecycle poller's
	// server listing and websocket reconnect in tests.
	listChannelStatesFn func() (map[string]*mattermost.Channel, bool)
	reconnectFn         func()
	// quietReconnect marks the next websocket close as deliberate (see
	// reconnectWebsocket), so it is not reported as a disconnect.
	quietReconnect atomic.Bool
	// queueRemoteEventFn overrides event delivery in tests. When nil, events go
	// to UserLogin.QueueRemoteEvent. The returned result is the framework's
	// answer (Queued, Ignored or Failed) and is used to decide whether a
	// deferred check will ever run.
	queueRemoteEventFn func(bridgev2.RemoteEvent) bridgev2.EventHandlingResult

	// lifecycleMu guards channelGen/deletedChannels/pendingChecks and makes the
	// generation check + deletion commit atomic with respect to restores.
	// It is separate from mu so holding it across queueEvent cannot deadlock
	// with the channel cache lock.
	lifecycleMu sync.Mutex
	// channelGen orders lifecycle events per channel: a newer create/update/
	// delete/restore invalidates checks and probes started for older events.
	channelGen map[string]uint64
	// pendingChecks records channels with a queued (not yet dequeued) deletion
	// check, so a burst of channel_deleted hints cannot queue more than one
	// bounded probe per channel and generation.
	pendingChecks map[string]bool
	// deletedChannels records portals whose deletion was committed in this
	// process, so a duplicate hint cannot delete twice.
	deletedChannels map[string]bool

	// previews caches resolved Mattermost permalink cards. A message must never
	// fail because a preview could not be built, so failures are cached too.
	previewMu sync.Mutex
	previews  map[string]cachedPreview

	// getTeamFn and teamSpacesFn override the team fetch and
	// network.team_spaces in tests (teams.go).
	getTeamFn    func(teamID string) (*mattermost.Team, error)
	teamSpacesFn func() bool
	// spaceChildrenFn overrides the "rooms still in this space" check.
	spaceChildrenFn func(teamID string) bool
	// uploadTeamIconFn, setSpaceStateFn, saveLoginFn and hasPortalFn override
	// the team login upkeep's side effects in tests (teamlogin_sync.go).
	uploadTeamIconFn func(team *mattermost.Team) (id.ContentURIString, error)
	setSpaceStateFn  func(evtType event.Type, content any)
	saveLoginFn      func()
	hasPortalFn      func(channelID string) bool

	// viewedAt is how far each channel is known read (see readsync.go).
	// Guarded by mu.
	viewedAt map[string]time.Time
	// teamLabels maps the account's team IDs to their names, for channel
	// names (teamnames.go). Guarded by mu.
	teamLabels map[string]string
	// commands caches each team's slash command triggers (commands.go).
	// Guarded by mu.
	commands map[string]*commandCache
	// Test hooks for slash commands.
	listCommandsFn   func(teamID string) ([]mattermost.Command, error)
	executeCommandFn func(channelID, teamID, rootID, command string) (*mattermost.CommandResponse, error)
	// muted is Mattermost's mute state per channel (mutes.go). Guarded by mu.
	muted map[string]bool
	// Test hooks for mutes.
	membershipsFn func(teamID string) ([]*mattermost.ChannelMember, error)
	muteRoomFn    func(channelID string, until time.Time) bool
	// fetchPreviewImageFn overrides downloading link preview images.
	fetchPreviewImageFn func(src string) ([]byte, string, error)
	// clearSpaceParentsFn overrides unlinking rooms from a team space.
	clearSpaceParentsFn func(teamID string)
	// viewChannelFn, markThreadReadFn and threadRootFn override the read
	// sync requests and thread lookup in tests.
	viewChannelFn    func(channelID string) error
	markThreadReadFn func(teamID, rootID string, ms int64) error
	threadRootFn     func(threadMXID string) string
}

var _ bridgev2.NetworkAPI = (*MattermostClient)(nil)
var _ bridgev2.BackfillingNetworkAPI = (*MattermostClient)(nil)
var _ bridgev2.RedactionHandlingNetworkAPI = (*MattermostClient)(nil)
var _ bridgev2.ReactionHandlingNetworkAPI = (*MattermostClient)(nil)
var _ bridgev2.TypingHandlingNetworkAPI = (*MattermostClient)(nil)
var _ bridgev2.EditHandlingNetworkAPI = (*MattermostClient)(nil)

// mmTypingEvent is a minimal bridgev2.RemoteTyping implementation.
type mmTypingEvent struct {
	channelID networkid.PortalID
	receiver  networkid.UserLoginID
	senderID  string
}

func (e *mmTypingEvent) GetPortalKey() networkid.PortalKey {
	return networkid.PortalKey{ID: e.channelID, Receiver: e.receiver}
}
func (e *mmTypingEvent) AddLogContext(c zerolog.Context) zerolog.Context { return c }
func (e *mmTypingEvent) GetSender() bridgev2.EventSender {
	return bridgev2.EventSender{Sender: networkid.UserID(e.senderID)}
}
func (e *mmTypingEvent) GetType() bridgev2.RemoteEventType { return bridgev2.RemoteEventTyping }
func (e *mmTypingEvent) ShouldCreatePortal() bool          { return false }
func (e *mmTypingEvent) GetTimeout() time.Duration         { return 10 * time.Second }

// mmChatDeleteEvent fires when a Mattermost channel is deleted.
type mmChatDeleteEvent struct {
	portalKey networkid.PortalKey
}

func (e *mmChatDeleteEvent) GetType() bridgev2.RemoteEventType {
	return bridgev2.RemoteEventChatDelete
}
func (e *mmChatDeleteEvent) GetPortalKey() networkid.PortalKey               { return e.portalKey }
func (e *mmChatDeleteEvent) AddLogContext(c zerolog.Context) zerolog.Context { return c }
func (e *mmChatDeleteEvent) GetSender() bridgev2.EventSender                 { return bridgev2.EventSender{} }
func (e *mmChatDeleteEvent) DeleteOnlyForMe() bool                           { return false }

// serverURL returns the effective REST base URL (may be overridden in tests).
func (c *MattermostClient) serverURL() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return c.ServerURL
}

// queueChannelResync queues a ChatResync event for a single Mattermost channel.
func (c *MattermostClient) queueChannelResync(ch *mattermost.Channel) {
	// Only this login's channels get rooms here (teamlogin.go). Every path
	// that creates a channel room goes through this.
	if !c.ownsChannel(ch) {
		return
	}
	c.queueEvent(c.channelResyncEvent(ch))
}

// channelResyncEvent builds the ChatResync that creates or refreshes a
// channel's room.
func (c *MattermostClient) channelResyncEvent(ch *mattermost.Channel) *simplevent.ChatResync {
	roomType := database.RoomTypeDefault
	switch {
	case ch.IsDM():
		roomType = database.RoomTypeDM
	case ch.IsGroupDM():
		roomType = database.RoomTypeGroupDM
	}

	var name *string
	if n := c.channelTitle(ch); n != "" {
		name = &n
	}

	return &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			CreatePortal: true,
			PortalKey:    networkid.PortalKey{ID: networkid.PortalID(ch.ID), Receiver: c.UserLogin.ID},
			LogContext:   func(z zerolog.Context) zerolog.Context { return z },
		},
		// Members are fetched when the portal handles the event, not here, so
		// the websocket loop never waits on the members request.
		GetChatInfoFunc: func(ctx context.Context, _ *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
			info := &bridgev2.ChatInfo{
				Name:        name,
				Type:        &roomType,
				Members:     c.chatMembers(ctx, ch), // caches the DM partner's profile
				CanBackfill: true,
				ParentID:    c.roomParent(ch), // the team's space, if on (teams.go)
				UserLocal:   c.userLocalInfo(ch.ID),
			}
			// A DM is titled after the other person, with their status.
			if title, topic := c.dmRoomInfo(ch); title != nil {
				info.Name, info.Topic = title, topic
				info.ExcludeChangesFromTimeline = true
				c.noteDMRoomInfo(ch.ID, title, topic)
			}
			return info, nil
		},
		CheckNeedsBackfillFunc: func(_ context.Context, _ *database.Message) (bool, error) { return true, nil },
	}
}

func (c *MattermostClient) Connect(ctx context.Context) {
	if c.UserID == "" {
		id, _, err := mattermost.GetSelf(c.serverURL(), c.Token)
		if err == nil {
			c.UserID = id
		}
	}
	// Before anything is synced, so names come out in the right format.
	c.resolveNameFormat(c.lifecycleLogger())
	c.done = make(chan struct{})
	c.startPresencePoller(c.done)
	go c.connectLoop()
	go c.lifecyclePollLoop()
}

func (c *MattermostClient) connectLoop() {
	backoff := 5 * time.Second
	failures := 0
	for {
		cl := mattermost.NewClient(c.serverURL(), c.Token)
		wsLog := c.lifecycleLogger()
		cl.Logf = func(format string, args ...any) { wsLog.Debug().Msgf(format, args...) }
		if err := cl.Connect(); err != nil {
			failures++
			c.UserLogin.BridgeState.Send(connectFailedState(failures, err))
		} else {
			failures = 0
			c.mu.Lock()
			c.client = cl
			c.mu.Unlock()
			// A new connection is up: a deliberate-reconnect mark that was never
			// consumed must not swallow the report of a later, real drop.
			c.quietReconnect.Store(false)
			c.processEvents() // blocks until WS closes
			backoff = 5 * time.Second
		}
		// Mattermost closes the WebSocket without an auth error event when the
		// token is expired or revoked, so check the token over REST before retrying.
		if _, _, err := mattermost.GetSelf(c.serverURL(), c.Token); errors.Is(err, mattermost.ErrUnauthorized) {
			c.UserLogin.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateBadCredentials,
				Error:      "mm-token-invalid",
				Message:    "Mattermost session expired or was revoked, please log in again",
			})
			return
		}
		select {
		case <-c.done:
			return
		case <-time.After(backoff):
			if backoff < 5*time.Minute {
				backoff *= 2
			}
		}
	}
}

func (c *MattermostClient) processEvents() {
	for raw := range c.client.Events {
		// Debug visibility: without this the connector is silent between the
		// websocket and the portal queue, which hides dropped lifecycle events.
		lg := c.lifecycleLogger()
		lg.Debug().Str("mm_event", fmt.Sprintf("%T", raw)).Msg("Mattermost event received")
		switch evt := raw.(type) {
		case *mattermost.HelloEvent:
			// When connected, load all channels and queue resyncs.
			c.UserLogin.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateConnected,
			})
			go c.syncChannels()

		case *mattermost.PostedEvent:
			if evt.Post == nil || !c.ownsPost(evt) {
				continue
			}
			// Restores arrive as a system post; treat it as proof the channel is
			// alive again so a pending (or already applied) deletion is undone.
			if evt.Post.Type == "system_channel_restored" {
				c.noteChannelAlive(evt.ChannelID)
				// Backstop for a missed channel_restored event: the room must be
				// writable before the notice below is relayed. Queued first, so
				// the portal lane applies it before the notice.
				c.queueAccessSync(evt.ChannelID)
			}
			// Pre-seed the channel cache from the inline WS metadata so that
			// GetChatInfo never races against a concurrent REST fetch.
			if evt.ChannelType != "" && evt.ChannelName != "" {
				c.mu.Lock()
				if c.channels == nil {
					c.channels = make(map[string]*mattermost.Channel)
				}
				if _, exists := c.channels[evt.ChannelID]; !exists {
					c.channels[evt.ChannelID] = &mattermost.Channel{
						ID:   evt.ChannelID,
						Type: evt.ChannelType,
						Name: evt.ChannelName,
					}
				}
				c.mu.Unlock()
			}
			sender := c.postSender(evt.Post)
			c.queueEvent(&simplevent.Message[*mattermost.Post]{
				EventMeta: simplevent.EventMeta{
					Type:         bridgev2.RemoteEventMessage,
					CreatePortal: true,
					PortalKey:    networkid.PortalKey{ID: networkid.PortalID(evt.ChannelID), Receiver: c.UserLogin.ID},
					Timestamp:    evt.Post.Timestamp(),
					Sender:       sender,
					LogContext:   func(z zerolog.Context) zerolog.Context { return z },
				},
				ID:   networkid.MessageID(evt.Post.ID),
				Data: evt.Post,
				ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *mattermost.Post) (*bridgev2.ConvertedMessage, error) {
					return c.convertPost(ctx, intent, portal.MXID, data)
				},
			})

		case *mattermost.PostEditedEvent:
			if evt.Post == nil {
				continue
			}
			isFromMe := evt.Post.UserID == c.UserID
			editSender := bridgev2.EventSender{
				Sender:   networkid.UserID(evt.Post.UserID),
				IsFromMe: isFromMe,
			}
			if isFromMe {
				editSender.SenderLogin = c.UserLogin.ID
			}
			c.queueEvent(&simplevent.Message[*mattermost.Post]{
				EventMeta: simplevent.EventMeta{
					Type:       bridgev2.RemoteEventEdit,
					PortalKey:  networkid.PortalKey{ID: networkid.PortalID(evt.ChannelID), Receiver: c.UserLogin.ID},
					Timestamp:  evt.Post.Timestamp(),
					Sender:     editSender,
					LogContext: func(z zerolog.Context) zerolog.Context { return z },
				},
				TargetMessage: networkid.MessageID(evt.Post.ID),
				Data:          evt.Post,
				ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, data *mattermost.Post) (*bridgev2.ConvertedEdit, error) {
					var parts []*bridgev2.ConvertedEditPart
					for _, dbMsg := range existing {
						content := c.editContent(ctx, data)
						parts = append(parts, &bridgev2.ConvertedEditPart{
							Part:    dbMsg,
							Type:    event.EventMessage,
							Content: content,
						})
						break // only edit the first/text part
					}
					return &bridgev2.ConvertedEdit{ModifiedParts: parts}, nil
				},
			})

		case *mattermost.PostDeletedEvent:
			if evt.Post == nil {
				continue
			}
			c.queueEvent(&simplevent.MessageRemove{
				EventMeta: simplevent.EventMeta{
					Type:       bridgev2.RemoteEventMessageRemove,
					PortalKey:  networkid.PortalKey{ID: networkid.PortalID(evt.ChannelID), Receiver: c.UserLogin.ID},
					Sender:     bridgev2.EventSender{IsFromMe: false},
					LogContext: func(z zerolog.Context) zerolog.Context { return z },
				},
				TargetMessage: networkid.MessageID(evt.Post.ID),
			})

		case *mattermost.ReactionEvent:
			evtType := bridgev2.RemoteEventReaction
			if !evt.IsAdd {
				evtType = bridgev2.RemoteEventReactionRemove
			}
			c.queueEvent(&simplevent.Reaction{
				EventMeta: simplevent.EventMeta{
					Type:       evtType,
					PortalKey:  networkid.PortalKey{ID: networkid.PortalID(evt.ChannelID), Receiver: c.UserLogin.ID},
					Sender:     bridgev2.EventSender{Sender: networkid.UserID(evt.Reaction.UserID)},
					LogContext: func(z zerolog.Context) zerolog.Context { return z },
				},
				TargetMessage: networkid.MessageID(evt.Reaction.PostID),
				EmojiID:       networkid.EmojiID(evt.Reaction.EmojiName),
				Emoji:         mmEmojiToUnicode(evt.Reaction.EmojiName),
			})

		case *mattermost.TypingEvent:
			if evt.UserID == c.UserID {
				continue // don't echo own typing
			}
			portalKey := networkid.PortalKey{ID: networkid.PortalID(evt.ChannelID), Receiver: c.UserLogin.ID}
			c.queueEvent(&mmTypingEvent{
				channelID: networkid.PortalID(evt.ChannelID),
				receiver:  c.UserLogin.ID,
				senderID:  evt.UserID,
			})
			// Mark portal as read on behalf of the logged-in user when
			// someone else starts typing — they're clearly looking at it.
			c.queueEvent(&simplevent.Receipt{
				EventMeta: simplevent.EventMeta{
					Type:       bridgev2.RemoteEventReadReceipt,
					PortalKey:  portalKey,
					Sender:     bridgev2.EventSender{IsFromMe: true, SenderLogin: c.UserLogin.ID},
					LogContext: func(z zerolog.Context) zerolog.Context { return z },
				},
				ReadUpTo: time.Now(),
			})

		case *mattermost.StatusChangeEvent:
			// Only the logged-in user's own status reaches this connection, so the
			// event is a hint to refresh presence early (presence.go).
			c.handleStatusChange(evt)

		case *mattermost.ChannelCreatedEvent:
			if evt.Channel == nil || !c.ownsChannel(evt.Channel) {
				continue
			}
			c.mu.Lock()
			if c.channels == nil {
				c.channels = make(map[string]*mattermost.Channel)
			}
			c.channels[evt.Channel.ID] = evt.Channel
			c.mu.Unlock()
			// A (re)created channel supersedes any pending deletion verdict and
			// must be bridgeable again even if its portal was deleted before.
			c.noteChannelAlive(evt.Channel.ID)
			c.queueChannelResync(evt.Channel)

		case *mattermost.ChannelUpdatedEvent:
			if evt.Channel == nil {
				continue
			}
			c.mu.Lock()
			if c.channels != nil {
				c.channels[evt.Channel.ID] = evt.Channel
			}
			c.mu.Unlock()
			newName := c.channelTitle(evt.Channel)
			// Updates can arrive out of order. Only an explicit restoration or
			// fresh provider state supersedes a pending deletion check.
			c.queueEvent(&simplevent.ChatInfoChange{
				EventMeta: simplevent.EventMeta{
					Type:       bridgev2.RemoteEventChatInfoChange,
					PortalKey:  networkid.PortalKey{ID: networkid.PortalID(evt.Channel.ID), Receiver: c.UserLogin.ID},
					LogContext: func(z zerolog.Context) zerolog.Context { return z },
				},
				ChatInfoChange: &bridgev2.ChatInfoChange{
					ChatInfo: &bridgev2.ChatInfo{Name: &newName},
				},
			})

		case *mattermost.ChannelRestoredEvent:
			// Invalidate pending removal checks. The corresponding Mattermost
			// system post supplies the user-visible message exactly once.
			c.noteChannelAlive(evt.ChannelID)
			// Make the room writable again. Queued here, ahead of the restore
			// post that follows, so the "unarchived" notice (sent from the
			// user's own account) is not blocked by the read-only levels.
			c.queueAccessSync(evt.ChannelID)

		case *mattermost.ChannelDeletedEvent:
			c.mu.Lock()
			if c.channels != nil {
				delete(c.channels, evt.ChannelID)
			}
			c.mu.Unlock()
			// The event alone does not prove the channel is gone: Mattermost
			// sends it for restorable archives too. Verify over REST first and
			// fail closed on any ambiguous answer.
			c.handleChannelDeleted(evt.ChannelID)
			// If it is only archived, the room stays but becomes read-only.
			c.queueAccessSync(evt.ChannelID)

		case *mattermost.ChannelMemberEvent:
			c.handleChannelMemberEvent(evt)

		case *mattermost.DirectAddedEvent:
			if !c.ownsDMs() {
				continue
			}
			// A new DM was opened. Fetch channel info and queue a resync.
			go func(chanID string) {
				ch, err := mattermost.GetChannel(c.serverURL(), c.Token, chanID)
				if err != nil {
					return
				}
				c.mu.Lock()
				if c.channels == nil {
					c.channels = make(map[string]*mattermost.Channel)
				}
				c.channels[ch.ID] = ch
				c.mu.Unlock()
				c.queueChannelResync(ch)
			}(evt.ChannelID)

		case *mattermost.UserUpdatedEvent:
			if evt.User == nil {
				continue
			}
			c.handleUserUpdated(evt.User)

		case *mattermost.TeamUpdatedEvent:
			c.queueTeamResync(evt.Team)

		case *mattermost.ChannelsViewedEvent:
			c.handleChannelsViewed(evt)

		case *mattermost.ChannelMemberUpdatedEvent:
			c.handleMemberUpdated(context.Background(), evt.Member)

		case *mattermost.EphemeralEvent:
			c.relayEphemeral(evt.Post)

		case *mattermost.AuthErrorEvent:
			c.UserLogin.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateBadCredentials,
				Error:      "mm-auth-error",
				Message:    evt.Message,
			})
			go c.Disconnect()
		}
	}

	// Events channel closed — WebSocket disconnected.
	if state, report := c.disconnectState(); report {
		c.UserLogin.BridgeState.Send(state)
	}
}

// disconnectState is the bridge state to report when the websocket closes. A
// close the connector asked for (reconnectWebsocket) reports nothing: the new
// connection announces "connected" within seconds, and an error state in between
// only shows up as a flicker. The mark is consumed, so a real drop that follows
// is reported as usual, as a transient disconnect (see connection_state.go).
func (c *MattermostClient) disconnectState() (status.BridgeState, bool) {
	if c.quietReconnect.Swap(false) {
		return status.BridgeState{}, false
	}
	return websocketDisconnectedState(), true
}

// syncChannels fetches all teams + channels for the user and queues ChatResync events.
func (c *MattermostClient) syncChannels() {
	teams, err := mattermost.GetTeams(c.serverURL(), c.Token, c.UserID)
	if err != nil {
		return
	}

	seenChannels := make(map[string]bool)
	foreign := make(map[string]bool) // listed, but bridged by another login
	listingComplete := true
	mutesComplete := true

	c.noteTeams(teams)
	c.pinLegacyLogin(teams)
	c.syncLoginProfile(teams)

	// Spaces first, so channels have their parent space when they sync.
	c.syncTeams(c.ownTeams(teams))

	for _, team := range teams {
		channels, err := mattermost.GetChannelsForUser(c.serverURL(), c.Token, c.UserID, team.ID)
		if err != nil {
			listingComplete = false
			continue
		}

		if !c.syncMemberships(team.ID) {
			mutesComplete = false
		}

		var owned []*mattermost.Channel
		for _, ch := range channels {
			if c.ownsChannel(ch) {
				owned = append(owned, ch)
			} else {
				foreign[ch.ID] = true
			}
		}

		c.mu.Lock()
		if c.channels == nil {
			c.channels = make(map[string]*mattermost.Channel, len(owned))
		}
		for _, ch := range owned {
			c.channels[ch.ID] = ch
		}
		c.mu.Unlock()

		for _, ch := range owned {
			if seenChannels[ch.ID] {
				continue // DMs appear in multiple teams
			}
			seenChannels[ch.ID] = true
			c.queueChannelResync(ch)
			c.queueAccessSync(ch.ID)
		}
	}

	c.syncExistingMutes(context.Background(), listingComplete && mutesComplete)

	// Rooms this login has for other logins' channels (made before teams
	// were per login) are removed; the owning login has its own.
	c.removeForeignPortals(foreign)
	for id := range foreign {
		seenChannels[id] = true // not this login's to reconcile
	}

	// Mattermost's channel listing omits archived channels, so their rooms are
	// never resynced above. Sync their access (read-only) from the live state
	// instead. Skipped when any team failed to list: an incomplete listing
	// must not be mistaken for "these channels are archived".
	if listingComplete {
		c.reconcileUnlistedPortals(seenChannels)
	}
}

func (c *MattermostClient) Disconnect() {
	if c.done != nil {
		close(c.done)
	}
	c.mu.RLock()
	cl := c.client
	c.mu.RUnlock()
	if cl != nil {
		cl.Close()
	}
}

func (c *MattermostClient) IsLoggedIn() bool {
	return c.Token != ""
}

func (c *MattermostClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
}

func (c *MattermostClient) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	if c.UserID != "" {
		return string(userID) == c.UserID
	}
	// UserID hasn't been resolved yet (e.g. during early login); fall back to
	// comparing against Username so self-detection still works.
	return string(userID) == c.Username
}

// dmOtherUserID extracts the other participant's ID from a DM channel name.
// Mattermost DM channel names are "<userID1>__<userID2>".
func dmOtherUserID(channelName, ownUserID string) string {
	if len(channelName) < 54 { // 26 + 2 + 26 minimum
		return ""
	}
	const sep = "__"
	idx := len(channelName)/2 - 1
	// find the __ separator around the middle
	for i := 0; i < len(channelName)-1; i++ {
		if channelName[i] == '_' && channelName[i+1] == '_' {
			idx = i
			break
		}
	}
	if idx <= 0 || idx+2 >= len(channelName) {
		return ""
	}
	_ = sep
	part1 := channelName[:idx]
	part2 := channelName[idx+2:]
	if part1 == ownUserID {
		return part2
	}
	return part1
}

// resolveDisplayName returns a cached or freshly fetched display name for a Mattermost user.
func (c *MattermostClient) resolveDisplayName(userID string) string {
	c.mu.RLock()
	u, ok := c.users[userID]
	c.mu.RUnlock()
	if ok {
		return c.displayName(u)
	}
	// Fetch and cache.
	fetched, err := mattermost.GetUser(c.serverURL(), c.Token, userID)
	if err != nil {
		return userID
	}
	c.mu.Lock()
	if c.users == nil {
		c.users = make(map[string]*mattermost.User)
	}
	c.users[userID] = fetched
	c.mu.Unlock()
	return c.displayName(fetched)
}

// sendNotice queues a plain-text notice from the bridge into a channel portal.
func (c *MattermostClient) sendNotice(channelID networkid.PortalID, body string) {
	c.queueNotice(channelID, "", body, false)
}

// queueNotice queues a notice from the bridge into a channel portal, in the
// thread of post rootID when set. markdown renders text as Markdown.
func (c *MattermostClient) queueNotice(channelID networkid.PortalID, rootID, body string, markdown bool) {
	c.queueEvent(&simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			CreatePortal: false,
			PortalKey:    networkid.PortalKey{ID: channelID, Receiver: c.UserLogin.ID},
			Timestamp:    time.Now(),
			Sender:       bridgev2.EventSender{},
			LogContext:   func(z zerolog.Context) zerolog.Context { return z },
		},
		ID:   networkid.MessageID(fmt.Sprintf("notice:%s:%d", channelID, time.Now().UnixMilli())),
		Data: body,
		ConvertMessageFunc: func(_ context.Context, _ *bridgev2.Portal, _ bridgev2.MatrixAPI, text string) (*bridgev2.ConvertedMessage, error) {
			content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
			if markdown {
				rendered := format.RenderMarkdown(text, true, false)
				rendered.MsgType = event.MsgNotice
				content = &rendered
			}
			converted := &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
				ID:      networkid.PartID(""),
				Type:    event.EventMessage,
				Content: content,
			}}}
			if rootID != "" {
				root := networkid.MessageID(rootID)
				converted.ThreadRoot = &root
			}
			return converted, nil
		},
	})
}

// editContent is the new content for an edited post. Linked posts are quoted
// again, so an edit does not drop the quote the original message carried.
func (c *MattermostClient) editContent(ctx context.Context, post *mattermost.Post) *event.MessageEventContent {
	text := postMarkdown(post)
	rendered := c.renderPostText(ctx, text)
	content := &rendered
	c.attachPermalinkQuotes(ctx, content, text)
	return content
}

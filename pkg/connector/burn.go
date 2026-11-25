package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Burn-on-read (Mattermost's self-deleting message) in Beeper.
//
// Mattermost keeps the text of a burn-on-read post apart from the post. A
// recipient's copy is empty until they reveal it, which tells the author it was
// read and starts that recipient's countdown. The author's own sessions (the
// bridge's login is one of them) receive the text at once. The bridge therefore
// treats the two directions differently:
//
//   - A post the bridge user wrote is shown with its text and disappears when
//     the post expires, like any other network's disappearing message.
//   - A post someone else wrote becomes a placeholder. The text is fetched only
//     when the user reacts to it with any emoji, which is the explicit reveal
//     (see reactionIntent); it then replaces the placeholder and disappears
//     when the countdown ends.
//   - Beeper's disappearing-message setting (after_read, one timer equal to the
//     server's burn duration) sends a message as burn-on-read.
//
// Nothing here ever reveals a post on its own, and the plaintext of someone
// else's post is never read except as the result of that reaction.

const (
	// revealEmoji is the icon the placeholder leads with. Any reaction reveals it;
	// the icon only makes the message easy to recognise in a hurry.
	revealEmoji = "🔥"
)

var errBurnEdit = errors.New("burn-on-read posts cannot be edited")

// setBurnSettings stores the server's burn-on-read settings.
func (c *MattermostClient) setBurnSettings(s mattermost.BurnOnReadSettings) { c.burn.Store(&s) }

// burnSettings returns the last known settings; the zero value (disabled)
// until the server has answered.
func (c *MattermostClient) burnSettings() mattermost.BurnOnReadSettings {
	if s := c.burn.Load(); s != nil {
		return *s
	}
	return mattermost.BurnOnReadSettings{}
}

// burnSettingsRefresh is how often the settings are re-read while connected, so
// an admin turning the feature off (or changing its duration) takes effect
// without a reconnect.
const burnSettingsRefresh = 10 * time.Minute

// runBurnSettingsLoop re-reads the settings every interval until done closes.
func (c *MattermostClient) runBurnSettingsLoop(done <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			c.refreshBurnSettings()
		}
	}
}

// refreshBurnSettings reads the settings from the server. A failure leaves the
// previous value in place and is logged, so an unreachable server never turns
// the feature on by accident.
func (c *MattermostClient) refreshBurnSettings() {
	s, err := mattermost.GetBurnOnReadSettings(c.serverURL(), c.Token)
	if err != nil {
		lg := c.lifecycleLogger()
		lg.Warn().Err(err).Msg("Could not read the burn-on-read settings")
		return
	}
	c.setBurnSettings(s)
}

// burnDisappear is the timer for a placeholder or mirrored own post: it ends
// when the post expires. A post that carries no expiry is bounded by the
// server's maximum lifetime for burn-on-read posts, a server setting read with
// the others. If the server has not reported one there is no bound to apply and
// none is invented: such a placeholder holds no text, and the burn lifecycle
// events and reconcile pass remove it.
func (c *MattermostClient) burnDisappear(post *mattermost.Post) database.DisappearingSetting {
	at := post.BurnExpiresAt()
	if at.IsZero() {
		ttl := c.burnSettings().MaxTTL
		if ttl <= 0 {
			return database.DisappearingSetting{}
		}
		at = post.Timestamp().Add(ttl)
	}
	// Clamp: clock skew can put the expiry before the creation time, and a
	// negative timer would be shown to clients as such.
	return database.DisappearingSetting{
		Type:        event.DisappearingTypeAfterSend,
		Timer:       max(at.Sub(post.Timestamp()), 0),
		DisappearAt: at,
	}
}

// convertBurnOnRead converts a burn-on-read post. It is called before anything
// else looks at the post, so no other path (files, previews, quotes) can reach
// its content.
func (c *MattermostClient) convertBurnOnRead(ctx context.Context, post *mattermost.Post) *bridgev2.ConvertedMessage {
	replyTo, threadRoot := threadRelation(post)
	part := &bridgev2.ConvertedMessagePart{ID: networkid.PartID(""), Type: event.EventMessage,
		DBMetadata: &MessageMetadata{AckPromptHidden: c.ackPromptFor(post) == "", AckPromptTracked: true,
			AckPromptUnsafe: post.IsBurnOnRead() && post.RevealedForViewer()}}

	own := c.UserID != "" && post.UserID == c.UserID
	switch {
	case own && post.Message != "" && !post.BurnExpiresAt().IsZero():
		text := post.Message
		if len(resolveFiles(post)) > 0 {
			text += "\n\n(Attachments are not shown here. Open the message in Mattermost.)"
		}
		content := c.renderPostText(ctx, text)
		part.Content = &content
	case own:
		part.Content = &event.MessageEventContent{MsgType: event.MsgNotice, Body: "You sent a burn-on-read message."}
	default:
		part.Content = &event.MessageEventContent{MsgType: event.MsgNotice, Body: c.burnPlaceholder(post)}
	}
	return &bridgev2.ConvertedMessage{
		ReplyTo:    replyTo,
		ThreadRoot: threadRoot,
		Parts:      []*bridgev2.ConvertedMessagePart{part},
		Disappear:  c.burnDisappear(post),
	}
}

// burnPlaceholder is the placeholder for a burn-on-read post that has not been
// revealed, with the acknowledgement prompt when one is asked for.
func (c *MattermostClient) burnPlaceholder(post *mattermost.Post) string {
	return withAckPrompt(burnPlaceholderBody(post.BurnReadDuration()), c.ackPrompt(post, true))
}

func burnPlaceholderBody(read time.Duration) string {
	after := "shortly after"
	if read > 0 {
		after = humanDuration(read) + " later"
	}
	return fmt.Sprintf("%s Burn-on-read message. Any reaction reveals it. Then it deletes itself %s.", revealEmoji, after)
}

// humanDuration renders a whole number of minutes, hours or seconds.
func humanDuration(d time.Duration) string {
	plural := func(n int64, unit string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return plural(int64(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return plural(int64(d/time.Minute), "minute")
	default:
		return plural(int64(d/time.Second), "second")
	}
}

// existingHasMedia reports whether any stored part is known to carry media.
// MediaFileID is the exact signal; a stable attachment part ID and the legacy
// unsafe flag are conservative fallbacks for rows written before it existed.
func existingHasMedia(existing []*database.Message) bool {
	for _, row := range existing {
		if strings.HasPrefix(string(row.PartID), "att_") {
			return true
		}
		if old, ok := row.Metadata.(*MessageMetadata); ok && old != nil && (old.MediaFileID != "" || old.AckPromptUnsafe) {
			return true
		}
	}
	return false
}

// convertPostEdit converts an edited post. Burn-on-read posts are not
// editable on the server, so an edit event for one is refused rather than
// carried: it would otherwise pass the text through unguarded.
//
// An edit that carries attachments is rebuilt from those attachments and
// mapped onto the rows already stored, so media keeps its Matrix event. A
// metadata-only or empty update edits nothing: writing an empty text body
// would replace an image post with a blank message.
func (c *MattermostClient) convertPostEdit(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, existing []*database.Message, post *mattermost.Post) (*bridgev2.ConvertedEdit, error) {
	if post == nil || post.IsBurnOnRead() || post.IsSystem() {
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}
	if len(existing) == 0 {
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}
	files := resolveFiles(post)
	if len(files) > 0 {
		if intent == nil {
			return nil, errEditMediaUnavailable
		}
		parts, err := c.convertPostMediaEditParts(ctx, intent, roomID, post)
		if err != nil {
			return nil, err
		}
		return c.mapEditParts(post, existing, parts), nil
	}
	// A snapshot without file_ids may be partial. If any stored row is known
	// media (or is a legacy unsafe row that could be), its text cannot be
	// trusted to describe the surviving attachments: fail closed. An explicit
	// empty list is the server saying the attachments were removed.
	if post.FileIDs == nil && existingHasMedia(existing) {
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}

	content := c.editContent(ctx, post)
	if content == nil || (strings.TrimSpace(content.Body) == "" && content.URL == "" && content.File == nil) {
		// Nothing but metadata changed (or the post now has no visible
		// content). Leave the stored parts untouched.
		return nil, bridgev2.ErrIgnoringRemoteEvent
	}
	primary := existing[0]
	for _, dbMsg := range existing {
		if dbMsg.PartID == "" {
			primary = dbMsg
			break
		}
	}
	unsafe, revealedBurn := false, false
	if old, ok := primary.Metadata.(*MessageMetadata); ok && old != nil {
		unsafe = old.AckPromptUnsafe
		revealedBurn = old.AckPromptRevealedBurn
	}
	primary.Metadata = &MessageMetadata{AckPromptHidden: c.ackPromptFor(post) == "", AckPromptTracked: true,
		AckPromptUnsafe: unsafe, AckPromptRevealedBurn: revealedBurn}
	edit := &bridgev2.ConvertedEdit{ModifiedParts: []*bridgev2.ConvertedEditPart{{
		Part:    primary,
		Type:    event.EventMessage,
		Content: content,
	}}}
	// An explicitly empty file list means the remaining attachments were
	// removed. A missing list is a partial snapshot and deletes nothing.
	if post.FileIDs != nil && len(post.FileIDs) == 0 {
		for _, dbMsg := range existing {
			if dbMsg != primary && strings.HasPrefix(string(dbMsg.PartID), "att_") {
				edit.DeletedParts = append(edit.DeletedParts, dbMsg)
			}
		}
	}
	return edit, nil
}

// --- Beeper -> Mattermost -------------------------------------------------

// burnRequested reports whether a Matrix message should be sent as
// burn-on-read: the message's own disappearing timer wins, otherwise the
// room's. Only after_read maps; Mattermost has no after_send equivalent.
func burnRequested(msg *bridgev2.MatrixMessage) bool {
	if msg.Content != nil && msg.Content.BeeperDisappearingTimer != nil {
		return msg.Content.BeeperDisappearingTimer.Type == event.DisappearingTypeAfterRead
	}
	return msg.Portal != nil && msg.Portal.Portal != nil && msg.Portal.Disappear.Type == event.DisappearingTypeAfterRead
}

var _ bridgev2.DisappearTimerChangingNetworkAPI = (*MattermostClient)(nil)

// HandleMatrixDisappearingTimer accepts the room timer. Mattermost has no
// per-room setting to change, so the choice is only stored on the portal and
// decides how this bridge sends. The one timer on offer is the server's own
// burn duration, because a burn-on-read post has no per-post timer.
func (c *MattermostClient) HandleMatrixDisappearingTimer(ctx context.Context, msg *bridgev2.MatrixDisappearingTimer) (bool, error) {
	timer := msg.Content
	if timer == nil || timer.Type == event.DisappearingTypeNone || timer.Timer.Duration == 0 {
		msg.Portal.Disappear = database.DisappearingSetting{}
		return true, nil
	}
	s := c.burnSettings()
	switch {
	case !s.Enabled:
		return false, errors.New("burn-on-read is not enabled on this Mattermost server")
	case timer.Type != event.DisappearingTypeAfterRead:
		return false, errors.New("only a timer that starts when the message is read maps to burn-on-read")
	case timer.Timer.Duration != s.Duration:
		return false, fmt.Errorf("burn-on-read messages last %s after they are read on this server", humanDuration(s.Duration))
	}
	msg.Portal.Disappear = database.DisappearingSetting{Type: event.DisappearingTypeAfterRead, Timer: s.Duration}
	return true, nil
}

// burnTimerFeatures adds the burn timer to the room features when the server
// has burn-on-read on.
func (c *MattermostClient) burnTimerFeatures(f *event.RoomFeatures) {
	s := c.burnSettings()
	if !s.Enabled {
		return
	}
	f.DisappearingTimer = &event.DisappearingTimerCapability{
		Types:  []event.DisappearingType{event.DisappearingTypeAfterRead},
		Timers: []jsontime.Milliseconds{jsontime.MS(s.Duration)},
	}
	if f.State == nil {
		f.State = event.StateFeatureMap{}
	}
	f.State[event.StateBeeperDisappearingTimer.Type] = &event.StateFeatures{Level: event.CapLevelFullySupported}
}

// removePost bridges a Matrix redaction. A burn-on-read post is burned (the
// author's is deleted, a recipient's copy expires), because deleting someone
// else's post is not allowed and not what removing your copy means.
func (c *MattermostClient) removePost(postID string) error {
	post, err := mattermost.GetPost(c.serverURL(), c.Token, postID)
	if err == nil && post.IsBurnOnRead() {
		return mattermost.BurnPost(c.serverURL(), c.Token, postID)
	}
	return mattermost.DeletePost(c.serverURL(), c.Token, postID)
}

// --- explicit reveal ------------------------------------------------------

// revealedPost is a post's text after an explicit reveal and when it expires.
type revealedPost struct {
	Content  *event.MessageEventContent
	ExpireAt time.Time
	// PromptHidden records presentation only. The revealed text itself is
	// never stored outside the Matrix event.
	PromptHidden bool
}

// revealBurnOnRead reveals the post for the bridge user and renders its text.
func (c *MattermostClient) revealBurnOnRead(ctx context.Context, postID string) (*revealedPost, error) {
	post, err := mattermost.RevealPost(c.serverURL(), c.Token, postID)
	if err != nil {
		return nil, fmt.Errorf("could not reveal the message: %w", err)
	}
	prompt := c.ackPrompt(post, false)
	content := c.revealedBurnContent(ctx, post, prompt)
	return &revealedPost{Content: content, ExpireAt: post.RevealExpiresAt(), PromptHidden: prompt == ""}, nil
}

// revealedBurnContent rebuilds exactly what a reveal shows: the post's text,
// the attachment fallback when files exist, a stand-in for a textless reveal,
// and the acknowledgement prompt that currently applies. Callers must have
// established that the post is revealed and unexpired for this viewer; this
// function only renders, so the same builder is safe for the reveal edit and
// for a later prompt-only rewrite.
func (c *MattermostClient) revealedBurnContent(ctx context.Context, post *mattermost.Post, prompt string) *event.MessageEventContent {
	text := post.Message
	if len(resolveFiles(post)) > 0 {
		text += "\n\n(Attachments are not shown here. Open the message in Mattermost.)"
	}
	if strings.TrimSpace(text) == "" {
		text = "(This message has no text.)"
	}
	content := c.renderPostText(ctx, withAckPrompt(text, prompt))
	return &content
}

// applyReveal replaces the placeholder with the revealed text. The removal is
// scheduled first and the text is only sent once that has succeeded: the
// reveal already happened on the server and cannot be undone, so a copy that
// could outlive the countdown must never be created.
func (c *MattermostClient) applyReveal(ctx context.Context, portal *bridgev2.Portal, target *database.Message, rp *revealedPost) error {
	expire := rp.ExpireAt
	if expire.IsZero() {
		// The server always sends it; fall back to its burn duration rather than guess.
		if d := c.burnSettings().Duration; d > 0 {
			expire = time.Now().Add(d)
		}
	}
	if expire.IsZero() {
		return errors.New("the message was revealed in Mattermost, but its countdown is unknown, so it is not shown here")
	}
	if err := c.addDisappear(ctx, portal.MXID, target.MXID, expire); err != nil {
		return fmt.Errorf("the message was revealed in Mattermost, but its removal here could not be scheduled, so it is not shown here: %w", err)
	}
	c.queueEvent(&simplevent.Message[*revealedPost]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: portal.PortalKey,
			Sender:    bridgev2.EventSender{Sender: target.SenderID},
			Timestamp: time.Now(),
		},
		TargetMessage: target.ID,
		Data:          rp,
		ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, data *revealedPost) (*bridgev2.ConvertedEdit, error) {
			var parts []*bridgev2.ConvertedEditPart
			for _, dbMsg := range existing {
				dbMsg.Metadata = &MessageMetadata{AckPromptHidden: data.PromptHidden, AckPromptTracked: true,
					AckPromptUnsafe: true, AckPromptRevealedBurn: true}
				parts = append(parts, &bridgev2.ConvertedEditPart{Part: dbMsg, Type: event.EventMessage, Content: data.Content})
				break
			}
			return &bridgev2.ConvertedEdit{ModifiedParts: parts}, nil
		},
	})
	return nil
}

// addDisappear sets when one Matrix event disappears.
func (c *MattermostClient) addDisappear(ctx context.Context, roomID id.RoomID, eventID id.EventID, at time.Time) error {
	if c.addDisappearFn != nil {
		return c.addDisappearFn(ctx, roomID, eventID, at)
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DisappearLoop == nil {
		return errors.New("not attached to a bridge")
	}
	return c.UserLogin.Bridge.DisappearLoop.Add(ctx, &database.DisappearingMessage{
		RoomID:    roomID,
		EventID:   eventID,
		Timestamp: time.Now(),
		DisappearingSetting: database.DisappearingSetting{
			Type:        event.DisappearingTypeAfterSend,
			Timer:       max(time.Until(at), 0),
			DisappearAt: at,
		},
	})
}

// --- lifecycle events -----------------------------------------------------

// handleBurnLifecycle applies burn-on-read lifecycle events and reports
// whether the event was one. None of them carries content: a burned post is
// removed, and a reveal or an all-recipients-revealed notice only moves the
// time a message disappears.
func (c *MattermostClient) handleBurnLifecycle(ctx context.Context, raw any) bool {
	switch evt := raw.(type) {
	case *mattermost.PostBurnedEvent:
		if evt.ChannelID == "" || c.UserLogin == nil {
			return true // nothing to address the removal to
		}
		c.queueEvent(&simplevent.MessageRemove{
			EventMeta: simplevent.EventMeta{
				Type:       bridgev2.RemoteEventMessageRemove,
				PortalKey:  networkid.PortalKey{ID: networkid.PortalID(evt.ChannelID), Receiver: c.UserLogin.ID},
				Sender:     bridgev2.EventSender{IsFromMe: false},
				LogContext: func(z zerolog.Context) zerolog.Context { return z },
			},
			TargetMessage: networkid.MessageID(evt.PostID),
		})
	case *mattermost.PostRevealedEvent:
		// Only a reveal by this user of someone else's post matters. The event
		// for the author's own post is a recipient-count update.
		if evt.AuthorID != c.UserID && evt.ExpireAt > 0 {
			c.moveDisappear(ctx, evt.ChannelID, evt.PostID, time.UnixMilli(evt.ExpireAt))
		}
	case *mattermost.PostAllRevealedEvent:
		if evt.SenderExpireAt > 0 {
			c.moveDisappear(ctx, evt.ChannelID, evt.PostID, time.UnixMilli(evt.SenderExpireAt))
		}
	default:
		return false
	}
	return true
}

func (c *MattermostClient) moveDisappear(ctx context.Context, channelID, postID string, at time.Time) {
	schedule := c.scheduleDisappear
	if c.scheduleDisappearFn != nil {
		schedule = c.scheduleDisappearFn
	}
	if err := schedule(ctx, channelID, postID, at); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("post_id", postID).Msg("Could not update when a burn-on-read message disappears")
	}
}

// scheduleDisappear sets when the bridged copy of a post disappears.
func (c *MattermostClient) scheduleDisappear(ctx context.Context, channelID, postID string, at time.Time) error {
	if c.UserLogin == nil || c.UserLogin.Bridge == nil {
		return errors.New("not attached to a bridge")
	}
	br := c.UserLogin.Bridge
	msg, err := br.DB.Message.GetFirstPartByID(ctx, c.UserLogin.ID, networkid.MessageID(postID))
	if err != nil {
		return err
	}
	if msg == nil {
		return nil // never bridged, or already gone
	}
	portal, err := br.GetExistingPortalByKey(ctx, msg.Room)
	if err != nil || portal == nil || portal.MXID == "" {
		return err
	}
	return c.addDisappear(ctx, portal.MXID, msg.MXID, at)
}

// --- removal-time reconciliation ------------------------------------------

// Backfilled messages get their removal time from the bridge framework in a
// background step that reports nothing back. A burn-on-read copy without one
// would outlive the post, so the bridge checks for itself: every backfilled
// burn-on-read post is tracked, and once its message is stored the bridge
// confirms a removal time exists and adds it if not. Repairs are logged as
// warnings, so a framework regression shows up instead of being papered over.

const (
	burnReconcileEvery = 15 * time.Second
	// burnReconcileGrace is how long past its removal time an entry is kept
	// waiting for its message to be stored.
	burnReconcileGrace = time.Hour
)

func (c *MattermostClient) trackBurnRemoval(postID string, at time.Time) {
	c.burnMu.Lock()
	defer c.burnMu.Unlock()
	if c.burnPending == nil {
		c.burnPending = map[string]time.Time{}
	}
	c.burnPending[postID] = at
}

func (c *MattermostClient) pendingBurnRemovals() int {
	c.burnMu.Lock()
	defer c.burnMu.Unlock()
	return len(c.burnPending)
}

func (c *MattermostClient) runBurnReconcileLoop(done <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			ctx := context.Background()
			if c.UserLogin != nil && c.UserLogin.Log.GetLevel() != zerolog.Disabled {
				ctx = c.UserLogin.Log.WithContext(ctx)
			}
			c.reconcileBurnRemovals(ctx)
		}
	}
}

// reconcileBurnRemovals makes one pass over the tracked posts.
func (c *MattermostClient) reconcileBurnRemovals(ctx context.Context) {
	c.burnMu.Lock()
	snapshot := make(map[string]time.Time, len(c.burnPending))
	for k, v := range c.burnPending {
		snapshot[k] = v
	}
	c.burnMu.Unlock()

	forget := func(postID string) {
		c.burnMu.Lock()
		delete(c.burnPending, postID)
		c.burnMu.Unlock()
	}
	log := zerolog.Ctx(ctx)
	for postID, at := range snapshot {
		if time.Since(at) > burnReconcileGrace {
			forget(postID)
			continue
		}
		roomID, eventID, found, err := c.lookupBurnMessage(ctx, postID)
		if err != nil {
			log.Warn().Err(err).Str("post_id", postID).Msg("Could not look up a burn-on-read message to check its removal time")
			continue
		}
		if !found {
			continue // not stored yet
		}
		has, err := c.hasDisappear(ctx, eventID)
		if err != nil {
			log.Warn().Err(err).Str("post_id", postID).Msg("Could not check a burn-on-read message's removal time")
			continue
		}
		if !has {
			log.Warn().Str("post_id", postID).Time("disappear_at", at).Msg("A backfilled burn-on-read message had no removal time; adding it")
			if err := c.addDisappear(ctx, roomID, eventID, at); err != nil {
				log.Error().Err(err).Str("post_id", postID).Msg("Could not add a removal time to a burn-on-read message")
				continue
			}
		}
		forget(postID)
	}
}

func (c *MattermostClient) lookupBurnMessage(ctx context.Context, postID string) (id.RoomID, id.EventID, bool, error) {
	if c.burnLookupFn != nil {
		return c.burnLookupFn(ctx, postID)
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil {
		return "", "", false, errors.New("not attached to a bridge")
	}
	br := c.UserLogin.Bridge
	msg, err := br.DB.Message.GetFirstPartByID(ctx, c.UserLogin.ID, networkid.MessageID(postID))
	if err != nil || msg == nil {
		return "", "", false, err
	}
	portal, err := br.GetExistingPortalByKey(ctx, msg.Room)
	if err != nil || portal == nil || portal.MXID == "" {
		return "", "", false, err
	}
	return portal.MXID, msg.MXID, true, nil
}

func (c *MattermostClient) hasDisappear(ctx context.Context, eventID id.EventID) (bool, error) {
	if c.hasDisappearFn != nil {
		return c.hasDisappearFn(ctx, eventID)
	}
	if c.UserLogin == nil || c.UserLogin.Bridge == nil {
		return false, errors.New("not attached to a bridge")
	}
	var n int
	err := c.UserLogin.Bridge.DB.Database.QueryRow(ctx, "SELECT COUNT(*) FROM disappearing_message WHERE mxid=$1", eventID).Scan(&n)
	return n > 0, err
}

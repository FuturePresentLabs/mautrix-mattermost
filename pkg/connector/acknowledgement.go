package connector

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

const ackPrefix = "ack:"

// ackMark leads the label of an acknowledgement status. Beeper draws a reaction
// at emoji size whatever it holds, so the label is kept short: a green check,
// "by", and the name.
const ackMark = "✅"
const ackBatchSize = 64

var errReservedAckReaction = errors.New("reserved acknowledgement reaction ID")

func isAckReaction(emojiID networkid.EmojiID) bool {
	return strings.HasPrefix(string(emojiID), ackPrefix)
}

func isRealPostID(id networkid.MessageID) bool {
	return id != "" && !strings.ContainsFunc(string(id), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
}

func (c *MattermostClient) ackQueueSupported() bool {
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DB == nil || c.UserLogin.Bridge.Config == nil {
		return false
	}
	// Same rule as deferred deletion: the read-then-rewrite of a post's statuses
	// is only correct when portal events run one at a time on their lane.
	ok, _ := c.deferredDeletionSupported()
	return ok
}

func (c *MattermostClient) ackChannel(channelID string) *mattermost.Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ch := c.channels[channelID]
	// An inline posted event can seed the type without the team's identity.
	if ch != nil && (ch.TeamID != "" || ch.IsDM() || ch.IsGroupDM()) {
		return ch
	}
	return nil
}

func (c *MattermostClient) ackOwnsPortal(ctx context.Context, portal *bridgev2.Portal, key networkid.PortalKey) bool {
	if !c.ackQueueSupported() || portal.PortalKey != key || key.Receiver != c.UserLogin.ID ||
		portal.MXID == "" || !isRealPostID(networkid.MessageID(key.ID)) {
		return false
	}
	channelID := string(key.ID)
	ch := c.ackChannel(channelID)
	if ch == nil {
		if ctx.Err() != nil {
			return false
		}
		var err error
		ch, err = mattermost.GetChannel(c.serverURL(), c.Token, channelID)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Msg("Could not verify acknowledgement channel ownership")
			return false
		}
	}
	return ch.ID == channelID && (ch.TeamID != "" || ch.IsDM() || ch.IsGroupDM()) && c.ownsChannel(ch)
}

func (c *MattermostClient) handleAcknowledgement(ack *mattermost.AcknowledgementEvent) {
	if !c.ackQueueSupported() || !isRealPostID(networkid.MessageID(ack.PostID)) ||
		!isRealPostID(networkid.MessageID(ack.UserID)) || !isRealPostID(networkid.MessageID(ack.ChannelID)) {
		return
	}
	key := networkid.PortalKey{ID: networkid.PortalID(ack.ChannelID), Receiver: c.UserLogin.ID}
	c.queueAckSnapshot(key, nil, []networkid.MessageID{networkid.MessageID(ack.PostID)})
}

// A neutral remote event runs the read and diff on the same portal lane as
// post creation, deletion, and Matrix reaction removals.
func (c *MattermostClient) queueAckSnapshot(key networkid.PortalKey, roots, replies []networkid.MessageID) {
	br := c.UserLogin.Bridge
	portal, err := br.GetExistingPortalByKey(br.BackgroundCtx, key)
	if err != nil {
		c.UserLogin.Log.Err(err).Msg("Failed to look up acknowledgement portal")
	}
	if portal == nil || portal.PortalKey != key {
		return
	}
	ev := &simplevent.EventMeta{
		PortalKey: key, UncertainReceiver: true,
		PreHandleWithResultFunc: func(ctx context.Context, portal *bridgev2.Portal) *bridgev2.EventHandlingResult {
			result := bridgev2.EventHandlingResultIgnored
			if c.ackOwnsPortal(ctx, portal, key) {
				c.fetchAckSnapshots(ctx, portal, roots, replies)
				result = bridgev2.EventHandlingResultSuccess
			}
			return &result
		},
	}
	if result := c.queueEvent(ev); result.Error != nil {
		c.UserLogin.Log.Err(result.Error).Msg("Failed to queue acknowledgement reconciliation")
	}
}

func (c *MattermostClient) ackSource(ctx context.Context, portal *bridgev2.Portal, postID networkid.MessageID) *database.Message {
	msg, err := portal.Bridge.DB.Message.GetFirstPartByID(ctx, portal.Receiver, postID)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to read acknowledgement source mapping")
	}
	if msg == nil || msg.Room != portal.PortalKey || msg.HasFakeMXID() || msg.MXID == "" {
		return nil
	}
	return msg
}

func (c *MattermostClient) fetchAckSnapshots(ctx context.Context, portal *bridgev2.Portal, roots, replies []networkid.MessageID) {
	var ids []string
	sources := make(map[string]*database.Message, len(roots))
	for _, postID := range roots {
		if source := c.ackSource(ctx, portal, postID); source != nil {
			ids = append(ids, string(postID))
			sources[string(postID)] = source
		}
	}
	if len(ids) > 0 {
		posts, err := mattermost.GetPostsByIDsForAcknowledgements(c.serverURL(), c.Token, ids)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Msg("Could not verify batch of Mattermost acknowledgements")
		} else {
			for _, post := range posts {
				if post == nil || sources[post.ID] == nil || post.ChannelID != string(portal.ID) {
					continue
				}
				if post.RootID != "" && post.DeleteAt == 0 {
					replies = append(replies, networkid.MessageID(post.ID))
				} else {
					c.applyAckSnapshot(ctx, portal, sources[post.ID], post)
				}
			}
		}
	}
	for _, postID := range replies {
		if ctx.Err() != nil {
			return
		}
		source := c.ackSource(ctx, portal, postID)
		if source == nil {
			continue
		}
		post, err := mattermost.GetPostForAcknowledgements(c.serverURL(), c.Token, string(postID))
		if err != nil {
			zerolog.Ctx(ctx).Warn().Msg("Could not verify current Mattermost acknowledgements")
		} else {
			c.applyAckSnapshot(ctx, portal, source, post)
		}
	}
}

func ackActors(post *mattermost.Post) ([]*mattermost.Acknowledgement, bool) {
	if post == nil || post.DeleteAt != 0 {
		return nil, true
	}
	if post.Metadata == nil {
		return nil, false
	}
	seen := make(map[string]bool)
	var actors []*mattermost.Acknowledgement
	for _, ack := range post.Metadata.Acknowledgements {
		if ack == nil || ack.PostID != post.ID || !isRealPostID(networkid.MessageID(ack.UserID)) ||
			ack.AcknowledgedAt <= 0 || ack.ChannelID != "" && ack.ChannelID != post.ChannelID {
			return nil, false
		}
		if !seen[ack.UserID] {
			actors = append(actors, ack)
			seen[ack.UserID] = true
		}
	}
	return actors, true
}

// ackPromptMetadata records the presentation state at conversion time. Unsafe
// is decided here, not at reconciliation, because a later post snapshot may no
// longer list the files, quote props or previews that made the body special.
func ackPromptMetadata(c *MattermostClient, post *mattermost.Post, content *event.MessageEventContent) *MessageMetadata {
	meta := &MessageMetadata{AckPromptHidden: c.ackPromptFor(post) == "", AckPromptTracked: true}
	meta.AckPromptUnsafe = len(resolveFiles(post)) > 0 || quotedReplyTarget(post) != nil ||
		(content != nil && len(content.BeeperLinkPreviews) > 0)
	return meta
}

// reconcileAckPrompt keeps the bridge-owned instruction in step with the
// bridged account's own acknowledgement. The persisted flag is presentation
// state only: no post text is copied into the bridge database.
func (c *MattermostClient) reconcileAckPrompt(ctx context.Context, portal *bridgev2.Portal, source *database.Message, post *mattermost.Post) {
	if c.UserID == "" || portal == nil || source == nil || post == nil {
		return
	}
	meta, ok := source.Metadata.(*MessageMetadata)
	if !ok || meta == nil || !meta.AckPromptTracked {
		// Rows written before prompt state existed are never rewritten: the
		// bridge cannot tell whether their body contains the instruction.
		return
	}
	prompt := c.ackPromptFor(post)
	hidden := prompt == ""
	if hidden == meta.AckPromptHidden {
		return
	}
	log := zerolog.Ctx(ctx)
	var (
		content           *event.MessageEventContent
		revealExpire      time.Time
		revealDisappearAt time.Time
	)
	if meta.AckPromptRevealedBurn {
		// A revealed burn body is the one unsafe body with a rebuild path. It
		// is kept separate from the ordinary guard below so attachments,
		// quotes and previews never become editable by sharing a flag.
		var ok bool
		revealExpire, revealDisappearAt, ok = c.ackRevealedBurnDeadline(ctx, portal, source, post, time.Now())
		if !ok {
			log.Debug().Str("post_id", post.ID).Msg("Leaving acknowledgement prompt in place: the revealed burn text cannot be rebuilt safely")
			return
		}
		content = c.revealedBurnContent(ctx, post, prompt)
	} else {
		if meta.AckPromptUnsafe || !c.ackPromptEditable(post) {
			log.Debug().Str("post_id", post.ID).Msg("Leaving acknowledgement prompt in place: the body cannot be rebuilt safely")
			return
		}
		content = c.ackPromptContent(ctx, post, prompt)
	}
	// Never edit something the reader may no longer be able to see: a redacted
	// or expired source is not resurrected by a prompt update.
	if !ackEventVisible(ctx, portal.Bridge.Bot, portal.MXID, source.MXID) {
		return
	}
	intent, err := portal.Internal().GetIntentForMXID(ctx, source.SenderMXID)
	if err != nil {
		log.Err(err).Msg("Could not get the author intent for an acknowledgement prompt edit")
		return
	}
	if !ackIntentMatches(intent, source.SenderMXID) {
		log.Debug().Str("post_id", post.ID).Msg("Leaving acknowledgement prompt in place: the resolved intent is not the original author")
		return
	}
	// Re-check the deadline immediately before the send: the snapshot and the
	// edit are separated by intent resolution and rendering.
	if meta.AckPromptRevealedBurn {
		now := time.Now()
		if !revealExpire.After(now) || !revealDisappearAt.After(now) {
			log.Debug().Str("post_id", post.ID).Msg("Leaving acknowledgement prompt in place: the revealed burn text expired before the edit")
			return
		}
	}
	part := *source
	part.Metadata = &MessageMetadata{AckPromptHidden: hidden, AckPromptTracked: true, AckPromptUnsafe: meta.AckPromptUnsafe,
		AckPromptRevealedBurn: meta.AckPromptRevealedBurn}
	res := portal.Internal().SendConvertedEdit(ctx, c.UserLogin, source.ID, source.SenderID, &bridgev2.ConvertedEdit{
		ModifiedParts: []*bridgev2.ConvertedEditPart{{Part: &part, Type: event.EventMessage, Content: content}},
	}, intent, time.Now(), 0)
	if !res.Success {
		log.Err(res.Error).Str("post_id", post.ID).Msg("Could not update the acknowledgement prompt")
		// SendConvertedEdit persists only successfully sent parts. The copied
		// part leaves the original row untouched on failure, so a retry can run.
		return
	}
	// The part carried the new flag, so the framework's own row write is the
	// persistence step. Keep the in-memory row coherent without a second write.
	meta.AckPromptHidden = hidden
}

// ackIntentMatches requires the resolved intent to be the message's original
// author. Sending an edit as anyone else would either be ignored by clients or
// fabricate another account's action.
func ackIntentMatches(intent bridgev2.MatrixAPI, sender id.UserID) bool {
	return intent != nil && sender != "" && intent.GetMXID() == sender
}

// ackPromptEditable limits prompt edits to bodies the bridge can rebuild
// exactly. Attachments are excluded because their Matrix media URL cannot be
// reconstructed from the post without re-uploading, and a text-only edit would
// drop the file. A revealed burn-on-read text has its own validated path in
// ackRevealedBurnDeadline; it is never admitted here.
func (c *MattermostClient) ackPromptEditable(post *mattermost.Post) bool {
	if post == nil || post.DeleteAt != 0 || len(resolveFiles(post)) > 0 {
		return false
	}
	// A quoted reply carries top-level props the edit content cannot rebuild.
	if quotedReplyTarget(post) != nil {
		return false
	}
	if post.IsBurnOnRead() && post.RevealedForViewer() {
		return false
	}
	return true
}

// ackRevealedBurnDeadline is the distinct guard for the one unsafe body the
// bridge may still rewrite: text this account revealed from a burn-on-read
// post. The snapshot must be the authoritative per-viewer post, already
// revealed and still ahead of both the viewer's expiry and the removal time
// stored for the original event. Nothing is inserted or moved here: if either
// deadline is missing, past, or the stored timer would outlive the reveal, the
// body is left alone.
func (c *MattermostClient) ackRevealedBurnDeadline(ctx context.Context, portal *bridgev2.Portal, source *database.Message, post *mattermost.Post, now time.Time) (time.Time, time.Time, bool) {
	if post == nil || portal == nil || source == nil || portal.MXID == "" || source.MXID == "" {
		return time.Time{}, time.Time{}, false
	}
	expire := post.RevealExpiresAt()
	if !post.IsBurnOnRead() || !post.RevealedForViewer() || strings.TrimSpace(post.Message) == "" || post.DeleteAt != 0 ||
		len(resolveFiles(post)) > 0 || quotedReplyTarget(post) != nil ||
		expire.IsZero() || !expire.After(now) {
		return time.Time{}, time.Time{}, false
	}
	disappearAt, ok := c.existingDisappearAt(ctx, portal.MXID, source.MXID)
	if !ok || !disappearAt.After(now) {
		return time.Time{}, time.Time{}, false
	}
	// The stored timer must not outlive the reveal. An earlier stored bound is
	// fine and wins; a later one would keep the text past its expiry and cannot
	// be corrected here because this path never changes timers.
	if disappearAt.After(expire) {
		return time.Time{}, time.Time{}, false
	}
	return expire, disappearAt, true
}

// existingDisappearAt reads the removal time already scheduled for the
// original Matrix event. disappear_at is stored in Unix nanoseconds.
func (c *MattermostClient) existingDisappearAt(ctx context.Context, room id.RoomID, mxid id.EventID) (time.Time, bool) {
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DB == nil || room == "" || mxid == "" {
		return time.Time{}, false
	}
	var at int64
	err := c.UserLogin.Bridge.DB.Database.QueryRow(ctx,
		`SELECT COALESCE(disappear_at, 0) FROM disappearing_message
		 WHERE bridge_id=$1 AND mx_room=$2 AND mxid=$3`,
		c.UserLogin.Bridge.ID, room, mxid).Scan(&at)
	if err != nil || at <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, at), true
}

// ackPromptFor picks the wording that matches what the event currently shows:
// an unrevealed burn post explains the reveal-then-acknowledge sequence, every
// other post uses the ordinary instruction. A post that no longer asks for an
// acknowledgement has no prompt in either case.
func (c *MattermostClient) ackPromptFor(post *mattermost.Post) string {
	if post == nil || !post.RequestsAcknowledgement() {
		return ""
	}
	if post.IsBurnOnRead() && !post.RevealedForViewer() {
		return c.ackPrompt(post, true)
	}
	return c.ackPrompt(post, false)
}

// ackPromptContent rebuilds the visible body from the authoritative post. For
// an unrevealed burn-on-read post this is the placeholder, which holds no
// plaintext.
func (c *MattermostClient) ackPromptContent(ctx context.Context, post *mattermost.Post, prompt string) *event.MessageEventContent {
	if post.IsBurnOnRead() {
		return &event.MessageEventContent{MsgType: event.MsgNotice, Body: withAckPrompt(burnPlaceholderBody(post.BurnReadDuration()), prompt)}
	}
	text := postMarkdown(post)
	rendered := c.renderPostText(ctx, withAckPrompt(text, prompt))
	c.attachPermalinkQuotes(ctx, &rendered, text)
	return &rendered
}

func (c *MattermostClient) ackReaction(ack *mattermost.Acknowledgement) *bridgev2.BackfillReaction {
	name := strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		default:
			return r
		}
	}, c.resolveDisplayName(ack.UserID)))
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || name == ack.UserID {
		name = "Someone"
	}
	runes := []rune(name)
	if len(runes) > 64 {
		name = string(runes[:64]) + "…"
	}
	return &bridgev2.BackfillReaction{
		Sender: bridgev2.EventSender{}, EmojiID: networkid.EmojiID(ackPrefix + ack.UserID),
		Emoji: ackMark + " by " + name, Timestamp: time.UnixMilli(ack.AcknowledgedAt),
	}
}

func (c *MattermostClient) applyAckSnapshot(ctx context.Context, portal *bridgev2.Portal, source *database.Message, post *mattermost.Post) {
	if post != nil && (post.ID != string(source.ID) || post.ChannelID != string(portal.ID)) {
		return
	}
	log := zerolog.Ctx(ctx)
	actors, valid := ackActors(post)
	if !valid {
		log.Warn().Msg("Ignoring incomplete Mattermost acknowledgement snapshot")
		return
	}
	db, bot := portal.Bridge.DB.Reaction, portal.Bridge.Bot
	rows, err := db.GetAllToMessageBySender(ctx, portal.Receiver, source.ID, "")
	if err != nil {
		log.Err(err).Msg("Failed to read bridge acknowledgement reactions")
		return
	}
	existing := make(map[networkid.EmojiID]*database.Reaction)
	for _, row := range rows {
		// Matrix batch backfill may omit SenderMXID in its reaction mapping.
		if row.Room == portal.PortalKey && row.MessagePartID == source.PartID &&
			(row.SenderMXID == "" || row.SenderMXID == bot.GetMXID()) && isAckReaction(row.EmojiID) {
			existing[row.EmojiID] = row
		}
	}
	c.reconcileAckPrompt(ctx, portal, source, post)

	if len(actors) > 0 && !ackEventVisible(ctx, bot, portal.MXID, source.MXID) {
		return
	}
	meta := simplevent.EventMeta{PortalKey: portal.PortalKey}
	for _, actor := range actors {
		emojiID := networkid.EmojiID(ackPrefix + actor.UserID)
		if row := existing[emojiID]; row != nil {
			delete(existing, emojiID)
			evt, err := fetchMatrixEvent(ctx, bot, portal.MXID, row.MXID)
			if err != nil && !errors.Is(err, mautrix.MNotFound) {
				log.Err(err).Msg("Could not verify Matrix acknowledgement reaction")
				continue
			}
			if err == nil && evt != nil && evt.Unsigned.RedactedBecause == nil {
				continue
			}
			if err := db.Delete(ctx, row); err != nil {
				log.Err(err).Msg("Failed to clear redacted acknowledgement reaction mapping")
				continue
			}
		}
		reaction := c.ackReaction(actor)
		res := portal.Internal().HandleRemoteReaction(ctx, c.UserLogin, &simplevent.Reaction{
			EventMeta: meta.WithType(bridgev2.RemoteEventReaction), TargetMessage: source.ID,
			EmojiID: reaction.EmojiID, Emoji: reaction.Emoji,
		})
		if res.Error != nil {
			log.Err(res.Error).Msg("Failed to send acknowledgement reaction")
		}
	}
	for _, row := range existing {
		res := portal.Internal().HandleRemoteReactionRemove(ctx, c.UserLogin, &simplevent.Reaction{
			EventMeta: meta.WithType(bridgev2.RemoteEventReactionRemove), TargetMessage: source.ID, EmojiID: row.EmojiID,
		})
		if res.Error != nil {
			log.Err(res.Error).Msg("Failed to redact acknowledgement reaction")
		}
	}
}

// rawEventAPI is implemented by the bridge's real intent. Its GetEvent decrypts
// encrypted events, and fails with "can't decrypt the event" when the bridge
// discards keys after decrypting (encryption.delete_keys.ratchet_on_decrypt, the
// Beeper default). Every Beeper room is encrypted, so the decrypting call cannot
// be used to ask whether an event still exists.
type rawEventAPI interface {
	Matrix() *appservice.IntentAPI
}

// fetchMatrixEvent reads an event from the homeserver without decrypting it.
// That is enough to learn whether it was redacted, which is all these checks need.
func fetchMatrixEvent(ctx context.Context, bot bridgev2.MatrixAPI, room id.RoomID, mxid id.EventID) (*event.Event, error) {
	if raw, ok := bot.(rawEventAPI); ok {
		if api := raw.Matrix(); api != nil {
			return api.Client.GetEvent(ctx, room, mxid)
		}
	}
	return bot.GetEvent(ctx, room, mxid)
}

func ackEventVisible(ctx context.Context, bot bridgev2.MatrixAPI, room id.RoomID, mxid id.EventID) bool {
	evt, err := fetchMatrixEvent(ctx, bot, room, mxid)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Could not verify Matrix acknowledgement source visibility")
		return false
	}
	return evt != nil && evt.Unsigned.RedactedBecause == nil
}

// Reconnect walks real source mappings in bounded, receiver-scoped pages.
// It runs once per websocket Hello, independently of short-interval channel polling.
func (c *MattermostClient) reconcileAcknowledgements() {
	if !c.ackQueueSupported() {
		if c.UserLogin != nil {
			c.UserLogin.Log.Warn().Msg("Acknowledgement feedback requires serialized portal events")
		}
		return
	}
	br := c.UserLogin.Bridge
	ctx := br.BackgroundCtx
	type target struct {
		room  networkid.PortalID
		post  networkid.MessageID
		reply bool
	}
	var cursor target
	for ctx.Err() == nil {
		rows, err := br.DB.Query(ctx, `
			SELECT m.room_id, m.id, MAX(CASE WHEN COALESCE(m.thread_root_id, '') <> '' THEN 1 ELSE 0 END)
			FROM message m JOIN portal p
			  ON p.bridge_id=m.bridge_id AND p.id=m.room_id AND p.receiver=m.room_receiver
			WHERE m.bridge_id=$1 AND m.room_receiver=$2 AND p.mxid IS NOT NULL AND p.mxid<>''
			  AND m.mxid<>'' AND m.mxid NOT LIKE $3 AND m.id NOT LIKE '%:%'
			  AND (m.room_id>$4 OR (m.room_id=$4 AND m.id>$5))
			GROUP BY m.room_id, m.id ORDER BY m.room_id, m.id LIMIT $6
		`, br.ID, c.UserLogin.ID, database.FakeMXIDPrefix+"%", cursor.room, cursor.post, ackBatchSize)
		if err != nil {
			c.UserLogin.Log.Err(err).Msg("Failed to list source posts for acknowledgement reconciliation")
			return
		}
		var page []target
		for rows.Next() {
			var entry target
			var reply int
			if err = rows.Scan(&entry.room, &entry.post, &reply); err != nil {
				break
			}
			entry.reply = reply != 0
			page = append(page, entry)
		}
		err = errors.Join(err, rows.Err(), rows.Close())
		if err != nil {
			c.UserLogin.Log.Err(err).Msg("Failed to read source posts for acknowledgement reconciliation")
			return
		}
		for group := page; len(group) > 0; {
			room := group[0].room
			var roots, replies []networkid.MessageID
			for len(group) > 0 && group[0].room == room {
				entry := group[0]
				group = group[1:]
				if !isRealPostID(entry.post) {
					continue
				}
				if entry.reply {
					replies = append(replies, entry.post)
				} else {
					roots = append(roots, entry.post)
				}
			}
			if len(roots)+len(replies) > 0 {
				c.queueAckSnapshot(networkid.PortalKey{ID: room, Receiver: c.UserLogin.ID}, roots, replies)
			}
		}
		if len(page) < ackBatchSize {
			return
		}
		cursor = page[len(page)-1]
	}
}

func (c *MattermostClient) reconcileAckPost(ctx context.Context, portal *bridgev2.Portal, channelID string, post *mattermost.Post) {
	if post.Metadata == nil || !isRealPostID(networkid.MessageID(post.ID)) ||
		post.ChannelID != channelID || string(portal.ID) != channelID || !c.ackQueueSupported() {
		return
	}
	if source := c.ackSource(ctx, portal, networkid.MessageID(post.ID)); source != nil &&
		c.ackOwnsPortal(ctx, portal, portal.PortalKey) {
		c.applyAckSnapshot(ctx, portal, source, post)
	}
}

// Forward and thread backfill complete on the portal lane. Reuse the fetched
// page after persistence, including posts the framework deduplicated away.
// The independent backwards worker attaches annotations to its new batch.
func (c *MattermostClient) backfillAcknowledgements(ctx context.Context, params bridgev2.FetchMessagesParams, resp *bridgev2.FetchMessagesResponse, posts map[string]*mattermost.Post) {
	portal := params.Portal
	if !c.ackOwnsPortal(ctx, portal, portal.PortalKey) {
		return
	}
	if params.Forward {
		resp.CompleteCallback = func() {
			for _, post := range posts {
				if post == nil || post.ChannelID != string(portal.ID) || !isRealPostID(networkid.MessageID(post.ID)) {
					continue
				}
				if source := c.ackSource(ctx, portal, networkid.MessageID(post.ID)); source != nil {
					c.applyAckSnapshot(ctx, portal, source, post)
				}
			}
		}
		return
	}
	for _, msg := range resp.Messages {
		post := posts[string(msg.ID)]
		if post == nil || post.ChannelID != string(portal.ID) || !isRealPostID(msg.ID) {
			continue
		}
		if actors, valid := ackActors(post); valid {
			for _, actor := range actors {
				msg.Reactions = append(msg.Reactions, c.ackReaction(actor))
			}
		}
	}
}

// The prompt line tells a Beeper reader that a post is waiting for them to
// acknowledge it. Beeper has no acknowledge button, so without this the
// reaction gesture could not be discovered.
const (
	ackPromptLine     = ackMark + " Any reaction acknowledges."
	ackPromptBurnLine = ackMark + " Acknowledgement requested. After revealing it, react again to acknowledge."
)

// ackPrompt is the prompt line to show on a post, or "" when there is nothing
// for this account to acknowledge: the post asks for nothing, it is the
// account's own, or the account has already acknowledged it. unrevealed picks
// the wording for a burn-on-read placeholder, where the first reaction reveals.
func (c *MattermostClient) ackPrompt(post *mattermost.Post, unrevealed bool) string {
	if post == nil || !post.RequestsAcknowledgement() || (c.UserID != "" && post.UserID == c.UserID) || post.AcknowledgedBy(c.UserID) {
		return ""
	}
	if unrevealed {
		return ackPromptBurnLine
	}
	return ackPromptLine
}

// withAckPrompt appends the prompt to message text on its own paragraph.
func withAckPrompt(text, prompt string) string {
	switch {
	case prompt == "":
		return text
	case strings.TrimSpace(text) == "":
		return prompt
	}
	return text + "\n\n" + prompt
}

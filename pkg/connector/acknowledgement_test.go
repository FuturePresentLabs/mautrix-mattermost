package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

type ackMatrix struct {
	bridgev2.MatrixConnector
	bot   *ackBot
	ghost *ackBot
}

func (*ackMatrix) ParseGhostMXID(mxid id.UserID) (networkid.UserID, bool) {
	return "", mxid == "@alias:example.test"
}

func (*ackMatrix) Init(*bridgev2.Bridge)                                       {}
func (m *ackMatrix) BotIntent() bridgev2.MatrixAPI                             { return m.bot }
func (m *ackMatrix) GhostIntent(networkid.UserID) bridgev2.MatrixAPI           { return m.ghost }
func (*ackMatrix) SendBridgeStatus(context.Context, *status.BridgeState) error { return nil }
func (*ackMatrix) SendMessageStatus(context.Context, *bridgev2.MessageStatus, *bridgev2.MessageStatusEventInfo) {
}
func (*ackMatrix) NewUserIntent(context.Context, id.UserID, string) (bridgev2.MatrixAPI, string, error) {
	return nil, "", nil
}

type ackBot struct {
	bridgev2.MatrixAPI
	mu            sync.Mutex
	sent          []*event.ReactionEventContent
	edited        []*event.MessageEventContent
	redacted      []id.EventID
	failRedaction bool
	failMessage   bool
	missing       map[id.EventID]bool
	mxid          id.UserID
	// raw is the intent the real bridge exposes through Matrix(). Its homeserver
	// fixture serves events as encrypted, like every room on Beeper.
	raw *appservice.IntentAPI
}

func (b *ackBot) GetMXID() id.UserID { return b.mxid }

// GetEvent behaves like the real bridge intent on Beeper: every room is
// encrypted and the bridge ratchets and discards keys after decrypting
// (encryption.delete_keys.ratchet_on_decrypt), so it cannot decrypt an event.
func (*ackBot) GetEvent(context.Context, id.RoomID, id.EventID) (*event.Event, error) {
	return nil, errors.New("can't decrypt the event")
}

func (b *ackBot) Matrix() *appservice.IntentAPI { return b.raw }

func (b *ackBot) isRedacted(eventID id.EventID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.missing[eventID]
}
func (*ackBot) IsDoublePuppet() bool                                      { return false }
func (*ackBot) EnsureInvited(context.Context, id.RoomID, id.UserID) error { return nil }
func (b *ackBot) SendMessage(_ context.Context, _ id.RoomID, typ event.Type, content *event.Content, _ *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch typ {
	case event.EventReaction:
		b.sent = append(b.sent, content.Parsed.(*event.ReactionEventContent))
		return &mautrix.RespSendEvent{EventID: id.EventID(fmt.Sprintf("$status%d", len(b.sent)))}, nil
	case event.EventMessage:
		if b.failMessage {
			return nil, errors.New("test message send failure")
		}
		b.edited = append(b.edited, content.Parsed.(*event.MessageEventContent))
		return &mautrix.RespSendEvent{EventID: id.EventID(fmt.Sprintf("$edit%d", len(b.edited)))}, nil
	case event.EventRedaction:
		if b.failRedaction {
			return nil, errors.New("test redaction failure")
		}
		b.redacted = append(b.redacted, content.Parsed.(*event.RedactionEventContent).Redacts)
	default:
		return nil, fmt.Errorf("unexpected Matrix event type %v", typ)
	}
	return &mautrix.RespSendEvent{EventID: "$redaction"}, nil
}

type ackFixture struct {
	bridge                *bridgev2.Bridge
	bot                   *ackBot
	key                   networkid.PortalKey
	client                *MattermostClient
	source                *database.Message
	server                *httptest.Server
	snapshot              atomic.Value
	gets, batches, writes atomic.Int32
	reveals               atomic.Int32
	matrixDown            atomic.Bool
}

func newAckFixture(t *testing.T, root networkid.MessageID) *ackFixture {
	t.Helper()
	f := &ackFixture{
		bot: &ackBot{mxid: "@bridge:example.test", missing: make(map[id.EventID]bool)},
		key: networkid.PortalKey{ID: "channel", Receiver: "owner"},
	}
	f.snapshot.Store(ackSnapshot{})
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const marker = "/event/"
		i := strings.LastIndex(r.URL.Path, marker)
		if r.Method != http.MethodGet || i < 0 {
			http.NotFound(w, r)
			return
		}
		if f.matrixDown.Load() {
			http.Error(w, "homeserver unavailable", http.StatusInternalServerError)
			return
		}
		eventID := id.EventID(r.URL.Path[i+len(marker):])
		evt := map[string]any{"event_id": eventID, "type": "m.room.encrypted", "room_id": "!room:example.test",
			"sender": "@bridge:example.test", "origin_server_ts": 1, "content": map[string]any{}}
		if f.bot.isRedacted(eventID) {
			evt["unsigned"] = map[string]any{"redacted_because": map[string]any{"type": "m.room.redaction", "event_id": "$redaction"}}
		}
		_ = json.NewEncoder(w).Encode(evt)
	}))
	t.Cleanup(matrixServer.Close)
	rawClient, err := mautrix.NewClient(matrixServer.URL, "@bridge:example.test", "token")
	if err != nil {
		t.Fatal(err)
	}
	f.bot.raw = &appservice.IntentAPI{Client: rawClient}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot := f.snapshot.Load().(ackSnapshot)
		post := snapshot.post(string(root))
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v4/users/unknown":
			http.NotFound(w, r)
			return
		case "POST /api/v4/reactions", "DELETE /api/v4/users/self/posts/post/reactions/green_circle":
			f.writes.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		case "GET /api/v4/posts/post":
			// The lookup every reaction makes. This post is ordinary, so a
			// reaction on it stays an emoji.
			_ = json.NewEncoder(w).Encode(&mattermost.Post{ID: "post", ChannelID: "channel", UserID: "alice", Message: "hello"})
			return
		case "GET /api/v4/posts/post/thread":
			f.gets.Add(1)
			if r.URL.Query().Get("skipFetchThreads") != "true" {
				t.Error("acknowledgement snapshot must request just the target post")
			}
		case "POST /api/v4/posts/post/reveal":
			f.reveals.Add(1)
			http.Error(w, "reveal must not be called by acknowledgement handling", http.StatusForbidden)
			return
		case "POST /api/v4/posts/ids":
			f.batches.Add(1)
			var requested []string
			if err := json.NewDecoder(r.Body).Decode(&requested); err != nil || len(requested) > ackBatchSize {
				t.Errorf("invalid acknowledgement batch: %v", err)
			}
			posts := []*mattermost.Post{}
			for _, postID := range requested {
				p := &mattermost.Post{ID: postID, ChannelID: "channel", Metadata: &mattermost.PostMetadata{}}
				if postID == "post" {
					p = post
				}
				posts = append(posts, p)
			}
			_ = json.NewEncoder(w).Encode(posts)
			return
		case "GET /api/v4/channels/channel/posts":
		default:
			t.Errorf("unexpected provider request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if snapshot.status != 0 {
			w.WriteHeader(snapshot.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": snapshot.errorID, "status_code": snapshot.status})
			return
		}
		_ = json.NewEncoder(w).Encode(mattermost.PostList{
			Order: []string{post.ID}, Posts: map[string]*mattermost.Post{post.ID: post},
		})
	}))
	t.Cleanup(f.server.Close)
	// Production runs "sqlite3-fk-wal"; bridgev2 clears a removed message's
	// reaction rows only through the schema's ON DELETE CASCADE, so the fixture
	// must enforce foreign keys like production does.
	raw, err := dbutil.NewWithDialect(filepath.Join(t.TempDir(), "bridge.db")+"?_foreign_keys=on", "sqlite3")
	mustAck(t, err)
	f.bridge = bridgev2.NewBridge("test", raw, zerolog.New(zerolog.NewTestWriter(t)), &bridgeconfig.BridgeConfig{PortalEventBuffer: 64},
		&ackMatrix{bot: f.bot, ghost: &ackBot{mxid: "@ghost:example.test"}}, &MattermostConnector{Config: &Config{ExpireEphemeralNotices: true}},
		func(*bridgev2.Bridge) bridgev2.CommandProcessor { return nil })
	f.bridge.BackgroundCtx = t.Context()
	mustAck(t, f.bridge.DB.Upgrade(t.Context()))
	// Bot-authored rows use the empty sender ID; with foreign keys enforced
	// (as in production) that ghost row must exist before any message does.
	if _, err := f.bridge.GetGhostByID(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.bridge.DB.Close() })
	f.client = f.login(t, "owner", "alpha", true)
	f.addRoom(t, f.key, "!room:example.test")
	f.source = f.addMessage(t, f.key, "post", "$original", root)
	if root != "" {
		f.addMessage(t, f.key, root, "$root", "")
	}
	return f
}

type ackSnapshot struct {
	actors   []string
	channel  string
	deleted  bool
	status   int
	errorID  string
	message  string
	author   string
	asking   bool
	files    []string
	burn     bool
	quotedTo string
	// revealExpireAt is the per-viewer expiry the server reports after this
	// account revealed a burn-on-read post (ms since epoch).
	revealExpireAt int64
}

func (s ackSnapshot) post(root string) *mattermost.Post {
	channel := s.channel
	if channel == "" {
		channel = "channel"
	}
	post := &mattermost.Post{ID: "post", ChannelID: channel, RootID: root, UserID: s.author, Message: s.message,
		FileIDs: s.files, Metadata: &mattermost.PostMetadata{}}
	if s.asking {
		yes := true
		post.Metadata.Priority = &mattermost.PostPriority{RequestedAck: &yes}
	}
	if s.burn {
		post.Type = mattermost.PostTypeBurnOnRead
		post.Props = map[string]any{"read_duration": float64(60000)}
	}
	if s.revealExpireAt != 0 {
		post.Metadata.ExpireAt = s.revealExpireAt
	}
	if s.quotedTo != "" {
		if post.Props == nil {
			post.Props = map[string]any{}
		}
		post.Props[mattermost.QuotedReplyToProp] = s.quotedTo
	}
	if s.deleted {
		post.DeleteAt = 1
	}
	for _, actor := range s.actors {
		post.Metadata.Acknowledgements = append(post.Metadata.Acknowledgements, &mattermost.Acknowledgement{PostID: post.ID, UserID: actor, AcknowledgedAt: 1})
	}
	return post
}

func mustAck(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *ackFixture) login(t *testing.T, loginID networkid.UserLoginID, team string, ownsDM bool) *MattermostClient {
	t.Helper()
	user, err := f.bridge.GetUserByMXID(t.Context(), "@bob:example.test")
	mustAck(t, err)
	user.Permissions = bridgeconfig.PermissionLevelUser
	mustAck(t, f.bridge.DB.UserLogin.Insert(t.Context(), &database.UserLogin{ID: loginID, UserMXID: user.MXID,
		Metadata: &UserLoginMetadata{TeamID: team, DMs: ownsDM}}))
	login, err := f.bridge.GetExistingUserLoginByID(t.Context(), loginID)
	mustAck(t, err)
	t.Cleanup(login.BridgeState.Destroy)
	c := login.Client.(*MattermostClient)
	c.baseURL = f.server.URL
	c.Token = "fixture"
	c.UserID = "self"
	c.channels = map[string]*mattermost.Channel{"channel": {ID: "channel", TeamID: "alpha", Type: "O"}}
	c.users = map[string]*mattermost.User{"self": {Username: "Me"}, "alice": {Username: "Alice\u202e\n Example"}, "bob": {Username: "Bob"}}
	return c
}

func (f *ackFixture) addRoom(t *testing.T, key networkid.PortalKey, room id.RoomID) {
	t.Helper()
	mustAck(t, f.bridge.DB.Portal.Insert(t.Context(), &database.Portal{PortalKey: key, MXID: room, Metadata: &PortalMetadata{}}))
}

func (f *ackFixture) addMessage(t *testing.T, key networkid.PortalKey, msgID networkid.MessageID, mxid id.EventID, root networkid.MessageID) *database.Message {
	t.Helper()
	msg := &database.Message{ID: msgID, Room: key, MXID: mxid, SenderMXID: f.bot.GetMXID(), ThreadRoot: root, Timestamp: time.Now(),
		Metadata: &MessageMetadata{AckPromptTracked: true}}
	mustAck(t, f.bridge.DB.Message.Insert(t.Context(), msg))
	return msg
}

func runAckFrames(t *testing.T, c *MattermostClient, frames ...any) {
	t.Helper()
	c.client = mattermost.NewClient("", "")
	for _, frame := range frames {
		c.client.Events <- frame
	}
	close(c.client.Events)
	c.processEvents()
	drainAckLane(t, c)
}

// drainAckLane waits until every portal event queued so far has been handled.
// Portal events run one at a time in queue order, so a sentinel queued last is
// handled last. Tests that replace the queue have nothing to wait for.
func drainAckLane(t *testing.T, c *MattermostClient) {
	t.Helper()
	if c.queueRemoteEventFn != nil {
		return
	}
	done := make(chan struct{})
	key := networkid.PortalKey{ID: "channel", Receiver: c.UserLogin.ID}
	if res := c.queueEvent(&simplevent.EventMeta{
		PortalKey: key, UncertainReceiver: true,
		PostHandleFunc: func(context.Context, *bridgev2.Portal) { close(done) },
	}); res.Error != nil {
		t.Fatalf("queue lane sentinel: %v", res.Error)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("portal lane did not drain")
	}
}

func (f *ackFixture) reconcile(t *testing.T) {
	t.Helper()
	f.client.reconcileAcknowledgements()
	drainAckLane(t, f.client)
}

func ackFrame(add bool) *mattermost.AcknowledgementEvent {
	return &mattermost.AcknowledgementEvent{Acknowledgement: mattermost.Acknowledgement{
		PostID: "post", ChannelID: "channel", UserID: "alice", AcknowledgedAt: 1,
	}, IsAdd: add}
}

func ackFrameFor(user string, add bool) *mattermost.AcknowledgementEvent {
	frame := ackFrame(add)
	frame.UserID = user
	return frame
}

func (f *ackFixture) promptHidden(t *testing.T) bool {
	t.Helper()
	msg, err := f.bridge.DB.Message.GetFirstPartByID(t.Context(), f.key.Receiver, f.source.ID)
	mustAck(t, err)
	if msg == nil {
		t.Fatal("source message disappeared")
	}
	meta, ok := msg.Metadata.(*MessageMetadata)
	if !ok || meta == nil {
		t.Fatalf("source metadata = %#v, want *MessageMetadata", msg.Metadata)
	}
	return meta.AckPromptHidden
}

func editBody(c *event.MessageEventContent) string {
	if c.NewContent != nil {
		return c.NewContent.Body
	}
	return c.Body
}

// The instruction is bridge-owned presentation, not post text: it goes away as
// soon as this account acknowledges, comes back when the acknowledgement is
// withdrawn, and someone else's acknowledgement changes nothing.
func TestAcknowledgementPromptRejectsMismatchedIntent(t *testing.T) {
	f := newAckFixture(t, "")
	// The resolver recognizes this alias, but its intent belongs to a different MXID.
	f.source.SenderMXID = "@alias:example.test"
	mustAck(t, f.bridge.DB.Message.Update(t.Context(), f.source))
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "hello", actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	ghost := f.bridge.Matrix.(*ackMatrix).ghost
	if len(ghost.edited) != 0 || len(f.bot.edited) != 0 || f.promptHidden(t) {
		t.Fatal("a mismatched author intent edited the message or advanced prompt state")
	}
}

func TestAcknowledgementPromptIsRemovedAndRestored(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "Please confirm the schedule", actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 1 {
		t.Fatalf("acknowledging sent %d message edits, want one", len(f.bot.edited))
	}
	edit := f.bot.edited[0]
	if edit.RelatesTo.Type != event.RelReplace || edit.RelatesTo.EventID != f.source.MXID {
		t.Fatalf("edit targets %+v, want a replacement of %s", edit.RelatesTo, f.source.MXID)
	}
	if body := editBody(edit); body != "Please confirm the schedule" {
		t.Errorf("after acknowledging body = %q, want the post text without the prompt", body)
	}
	if !f.promptHidden(t) {
		t.Error("prompt state was not persisted as hidden")
	}
	// The same authoritative state delivered twice must not send a second edit.
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 1 {
		t.Fatalf("a repeated acknowledgement sent %d edits in total, want one", len(f.bot.edited))
	}

	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "Please confirm the schedule"})
	runAckFrames(t, f.client, ackFrameFor("self", false))
	if len(f.bot.edited) != 2 {
		t.Fatalf("withdrawing sent %d edits in total, want two", len(f.bot.edited))
	}
	if body := editBody(f.bot.edited[1]); body != "Please confirm the schedule\n\n"+promptLine {
		t.Errorf("after withdrawing body = %q, want the prompt back", body)
	}
	if f.promptHidden(t) {
		t.Error("prompt state was not persisted as shown")
	}

	// Someone else acknowledging does not hide this account's instruction.
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "Please confirm the schedule", actors: []string{"bob"}})
	runAckFrames(t, f.client, ackFrameFor("bob", true))
	if len(f.bot.edited) != 2 {
		t.Fatalf("another actor's acknowledgement changed the prompt: %d edits", len(f.bot.edited))
	}
	if f.promptHidden(t) {
		t.Error("another actor's acknowledgement hid the prompt")
	}
}

// A prompt edit must never be built from an attachment: replacing a media
// event with text would drop the file. The prompt stays until a real edit or
// backfill re-renders the caption from the authoritative post.
func TestAcknowledgementPromptLeavesAttachmentCaptionsAlone(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "caption", files: []string{"file1"}, actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 0 {
		t.Fatalf("sent %d edits for an attachment post, want none", len(f.bot.edited))
	}
	if f.promptHidden(t) {
		t.Error("prompt state changed for a post whose body was not edited")
	}
}

// Burn-on-read: the placeholder holds no plaintext, so its instruction may be
// changed; a revealed text is never rebuilt, re-fetched or re-timed.
func TestAcknowledgementPromptOnBurnPosts(t *testing.T) {
	t.Run("unrevealed placeholder", func(t *testing.T) {
		f := newAckFixture(t, "")
		f.snapshot.Store(ackSnapshot{asking: true, author: "alice", burn: true, actors: []string{"self"}})
		runAckFrames(t, f.client, ackFrameFor("self", true))
		if len(f.bot.edited) != 1 {
			t.Fatalf("sent %d edits for a placeholder, want one", len(f.bot.edited))
		}
		body := editBody(f.bot.edited[0])
		if strings.Contains(body, "Acknowledgement requested") || !strings.Contains(body, "Any reaction reveals it") {
			t.Errorf("placeholder body = %q, want the reveal instruction without the acknowledgement prompt", body)
		}
		if !strings.HasPrefix(string(f.bot.edited[0].MsgType), "m.notice") {
			t.Errorf("placeholder edit type = %q, want a notice", f.bot.edited[0].MsgType)
		}
		f.snapshot.Store(ackSnapshot{asking: true, author: "alice", burn: true})
		runAckFrames(t, f.client, ackFrameFor("self", false))
		if len(f.bot.edited) != 2 {
			t.Fatalf("withdrawing on a placeholder sent %d edits in total, want two", len(f.bot.edited))
		}
		body = editBody(f.bot.edited[1])
		if strings.Contains(body, promptLine) || !strings.HasSuffix(strings.TrimSpace(body), burnPromptLine) {
			t.Errorf("restored placeholder = %q, want the burn instruction %q", body, burnPromptLine)
		}
	})
	t.Run("revealed text", func(t *testing.T) {
		f := newAckFixture(t, "")
		f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: secretText, burn: true, actors: []string{"self"}})
		runAckFrames(t, f.client, ackFrameFor("self", true))
		if len(f.bot.edited) != 0 {
			t.Fatal("edited a revealed burn-on-read message, which would require holding its text")
		}
		if f.reveals.Load() != 0 {
			t.Fatal("acknowledgement handling revealed a burn-on-read post")
		}
	})
}

func TestAcknowledgementPromptSkipsDeletedAndUnverifiableMessages(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "hello", deleted: true, actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 0 {
		t.Fatal("edited a deleted post")
	}

	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "hello"})
	f.source.Metadata.(*MessageMetadata).AckPromptHidden = true
	mustAck(t, f.bridge.DB.Message.Update(t.Context(), f.source))
	f.matrixDown.Store(true)
	runAckFrames(t, f.client, ackFrameFor("self", false))
	if len(f.bot.edited) != 0 {
		t.Fatal("restored a prompt without being able to check the source")
	}
	if !f.promptHidden(t) {
		t.Fatal("prompt state changed although no edit was sent")
	}
}

// A failed edit leaves the persisted state unchanged so the next pass retries.
func TestAcknowledgementPromptEditFailureIsRetried(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "hello", actors: []string{"self"}})
	f.bot.failMessage = true
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if f.promptHidden(t) {
		t.Fatal("a failed edit marked the prompt hidden")
	}
	f.bot.failMessage = false
	f.reconcile(t)
	if !f.promptHidden(t) || len(f.bot.edited) != 1 {
		t.Fatalf("retry did not remove the prompt: hidden=%t edits=%d", f.promptHidden(t), len(f.bot.edited))
	}
}

// Withdrawing the request itself removes an instruction that is already shown.
func TestAcknowledgementPromptFollowsWithdrawnRequest(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{author: "alice", message: "hello", actors: []string{"alice"}})
	runAckFrames(t, f.client, ackFrame(true))
	if len(f.bot.edited) != 1 || editBody(f.bot.edited[0]) != "hello" {
		t.Fatalf("withdrawn request left the prompt in place: %v", f.bot.edited)
	}
	if !f.promptHidden(t) {
		t.Error("prompt state was not persisted after the request was withdrawn")
	}
}

// A row written before prompt state existed is never rewritten on a guess.
func TestAcknowledgementPromptLeavesLegacyRowsAlone(t *testing.T) {
	f := newAckFixture(t, "")
	f.source.Metadata = &MessageMetadata{}
	mustAck(t, f.bridge.DB.Message.Update(t.Context(), f.source))
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "hello", actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 0 {
		t.Fatalf("edited a legacy row: %v", f.bot.edited)
	}
}

// An edit must not change where the message lives or when it disappears.
func TestAcknowledgementPromptEditPreservesRelationsAndExpiry(t *testing.T) {
	f := newAckFixture(t, "root")
	f.source.ReplyTo = networkid.MessageOptionalPartID{MessageID: "root"}
	mustAck(t, f.bridge.DB.Message.Update(t.Context(), f.source))
	_, err := f.bridge.DB.Exec(t.Context(), `INSERT INTO disappearing_message
		(bridge_id, mx_room, mxid, timestamp, type, timer, disappear_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		f.bridge.ID, "!room:example.test", f.source.MXID, int64(1000), "after_send", int64(60000), int64(123456789))
	mustAck(t, err)
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "hello", actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 1 {
		t.Fatalf("sent %d edits, want one", len(f.bot.edited))
	}
	msg, err := f.bridge.DB.Message.GetFirstPartByID(t.Context(), f.key.Receiver, f.source.ID)
	mustAck(t, err)
	if msg == nil || msg.ThreadRoot != "root" || msg.ReplyTo.MessageID != "root" {
		t.Fatalf("edit changed the message relations: %+v", msg)
	}
	var disappearAt int64
	mustAck(t, f.bridge.DB.QueryRow(t.Context(), `SELECT disappear_at FROM disappearing_message WHERE mxid=$1`, f.source.MXID).Scan(&disappearAt))
	if disappearAt != 123456789 {
		t.Fatalf("edit changed the removal time to %d", disappearAt)
	}
	if got := f.bot.edited[0].RelatesTo; got.Type != event.RelReplace || got.EventID != f.source.MXID {
		t.Fatalf("edit relation = %+v, want a replacement of the original event only", got)
	}
}

// A quoted reply's top-level props cannot be rebuilt from the edit content, so
// the bridge leaves its instruction alone rather than dropping the quote.
func TestAcknowledgementPromptLeavesQuotedRepliesAlone(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "reply", quotedTo: "parent", actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 0 {
		t.Fatalf("edited a quoted reply: %v", f.bot.edited)
	}
}

// The unsafe marking is made at conversion, so a later snapshot that no longer
// lists the file cannot turn an attachment body into a text edit.
func TestAcknowledgementPromptUnsafeStateSurvivesSnapshotDrift(t *testing.T) {
	f := newAckFixture(t, "")
	f.source.Metadata = &MessageMetadata{AckPromptHidden: false, AckPromptTracked: true, AckPromptUnsafe: true}
	mustAck(t, f.bridge.DB.Message.Update(t.Context(), f.source))
	// The snapshot deliberately has no FileIDs, unlike the post at conversion.
	f.snapshot.Store(ackSnapshot{asking: true, author: "alice", message: "caption", actors: []string{"self"}})
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 0 {
		t.Fatalf("edited a body marked unsafe at conversion: %v", f.bot.edited)
	}
	if f.promptHidden(t) {
		t.Error("prompt state changed although no edit was sent")
	}
}

func TestAckIntentMustBeTheOriginalAuthor(t *testing.T) {
	bot := &ackBot{mxid: "@author:example.test"}
	if !ackIntentMatches(bot, "@author:example.test") {
		t.Fatal("matching author intent rejected")
	}
	if ackIntentMatches(bot, "@someone-else:example.test") {
		t.Fatal("mismatched intent accepted an edit as the wrong account")
	}
	if ackIntentMatches(nil, "@author:example.test") || ackIntentMatches(bot, "") {
		t.Fatal("missing intent or sender accepted")
	}
}

func (f *ackFixture) reaction(t *testing.T, emojiID networkid.EmojiID) *database.Reaction {
	t.Helper()
	row, err := f.bridge.DB.Reaction.GetByID(t.Context(), f.key.Receiver, f.source.ID, f.source.PartID, "", emojiID)
	mustAck(t, err)
	return row
}

// The framework appends an emoji presentation selector (U+FE0F) to the leading
// emoji when it sends a reaction to Matrix. It is invisible and not part of the
// label, so comparisons ignore it.
func withoutPresentationSelector(key string) string { return strings.ReplaceAll(key, "\ufe0f", "") }

func TestAcknowledgementFeedback(t *testing.T) {
	f := newAckFixture(t, "root")
	ordinary := &database.Reaction{Room: f.key, MessageID: f.source.ID, SenderMXID: f.bot.GetMXID(), EmojiID: "green_circle", MXID: "$ordinary"}
	f.bridge.GetGhostByID(t.Context(), "")
	mustAck(t, f.bridge.DB.Reaction.Upsert(t.Context(), ordinary))

	f.snapshot.Store(ackSnapshot{actors: []string{"alice", "bob", "unknown", "alice"}})
	runAckFrames(t, f.client, ackFrame(true), ackFrame(false))
	expected := []string{"Alice Example", "Bob", "Someone"}
	if len(f.bot.sent) != len(expected) {
		t.Fatalf("expected one annotation per actor, got %v", f.bot.sent)
	}
	for i, name := range expected {
		rel := f.bot.sent[i].RelatesTo
		if rel.Type != event.RelAnnotation || rel.EventID != f.source.MXID || withoutPresentationSelector(rel.Key) != "✅ by "+name {
			t.Fatalf("incorrect annotation: %+v", rel)
		}
	}
	alice, bob := f.reaction(t, "ack:alice"), f.reaction(t, "ack:bob")
	if alice == nil || bob == nil || alice.MXID == bob.MXID || alice.SenderID != "" || alice.SenderMXID != f.bot.GetMXID() {
		t.Fatalf("incorrect persisted bot reactions: %+v %+v", alice, bob)
	}
	f.snapshot.Store(ackSnapshot{actors: []string{"bob", "unknown"}})
	runAckFrames(t, f.client, ackFrame(true))
	if f.reaction(t, "ack:alice") != nil || f.reaction(t, "ack:bob") == nil || f.reaction(t, "green_circle") == nil ||
		len(f.bot.redacted) != 1 || f.bot.redacted[0] != alice.MXID {
		t.Fatal("withdrawal did not remove exactly Alice's status")
	}
	f.snapshot.Store(ackSnapshot{actors: []string{"alice", "bob", "unknown"}})
	runAckFrames(t, f.client, ackFrame(false), ackFrame(true))
	if reack := f.reaction(t, "ack:alice"); reack == nil || reack.MXID == alice.MXID || len(f.bot.sent) != 4 {
		t.Fatal("re-ack did not create exactly one fresh reaction under the stable ID")
	}
}

func TestAcknowledgementReconnectAndLocalRedaction(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}})
	for i := range ackBatchSize + 1 {
		f.addMessage(t, f.key, networkid.MessageID(fmt.Sprintf("history%03d", i)), id.EventID(fmt.Sprintf("$history%d", i)), "")
	}
	f.addMessage(t, f.key, "command:synthetic", "$command", "")
	f.addMessage(t, f.key, "fake", id.EventID(database.FakeMXIDPrefix+"event"), "")
	f.reconcile(t)
	row := f.reaction(t, "ack:alice")
	if row == nil || f.batches.Load() != 2 || f.gets.Load() != 0 {
		t.Fatalf("offline ack was not restored with two bounded root batches: %+v, batches=%d gets=%d", row, f.batches.Load(), f.gets.Load())
	}

	portal, err := f.bridge.GetExistingPortalByKey(t.Context(), f.key)
	mustAck(t, err)
	redaction := &event.Event{ID: "$local", Type: event.EventRedaction, Redacts: row.MXID, Content: event.Content{
		Parsed: &event.RedactionEventContent{Redacts: row.MXID},
	}}
	result := portal.Internal().HandleMatrixRedaction(t.Context(), f.client.UserLogin, nil, redaction)
	if !result.Success || f.reaction(t, "ack:alice") != nil {
		t.Fatalf("local redaction failed: %+v", result)
	}
	f.reconcile(t)
	if f.reaction(t, "ack:alice") == nil {
		t.Fatal("reconnect did not restore locally removed status")
	}
	f.snapshot.Store(ackSnapshot{})
	f.reconcile(t)
	if f.reaction(t, "ack:alice") != nil || f.writes.Load() != 0 {
		t.Fatal("missed withdrawal was not reconciled locally")
	}
}

func TestAcknowledgementFailureAndSourceDeletion(t *testing.T) {
	f := newAckFixture(t, "root")
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}})
	f.reconcile(t)
	if f.reaction(t, "ack:alice") == nil || f.gets.Load() != 1 {
		t.Fatal("reconnect did not restore the reply acknowledgement")
	}
	f.snapshot.Store(ackSnapshot{status: http.StatusServiceUnavailable})
	runAckFrames(t, f.client, ackFrame(false))
	if f.reaction(t, "ack:alice") == nil {
		t.Fatal("a failed snapshot removed the existing status")
	}
	f.snapshot.Store(ackSnapshot{})
	f.bot.failRedaction = true
	runAckFrames(t, f.client, ackFrame(false))
	if f.reaction(t, "ack:alice") == nil {
		t.Fatal("failed Matrix removal lost reaction mapping")
	}
	f.bot.failRedaction = false
	f.reconcile(t)
	if f.reaction(t, "ack:alice") != nil {
		t.Fatal("resync did not retry removal")
	}
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}})
	f.bot.missing[f.source.MXID] = true
	runAckFrames(t, f.client, ackFrame(true))
	f.reconcile(t)
	if len(f.bot.sent) != 1 {
		t.Fatal("reapplied acknowledgement to a redacted source")
	}
	delete(f.bot.missing, f.source.MXID)
	f.reconcile(t)
	runAckFrames(t, f.client, &mattermost.PostDeletedEvent{ChannelID: "channel", Post: &mattermost.Post{ID: "post", ChannelID: "channel"}})
	source, err := f.bridge.DB.Message.GetFirstPartByID(t.Context(), f.key.Receiver, f.source.ID)
	mustAck(t, err)
	if source != nil || f.reaction(t, "ack:alice") != nil || f.bot.redacted[len(f.bot.redacted)-1] != f.source.MXID {
		t.Fatal("source deletion did not redact the original and cascade the annotation mapping")
	}
	runAckFrames(t, f.client, ackFrame(true))
	if len(f.bot.sent) != 2 {
		t.Fatal("reapplied acknowledgement to an unmapped source")
	}
}

func TestAcknowledgementRouting(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}})
	wrong := ackFrame(true)
	wrong.ChannelID = "missing"
	runAckFrames(t, f.client, wrong)
	missing, err := f.bridge.DB.Portal.GetByKey(t.Context(), networkid.PortalKey{ID: "missing", Receiver: f.key.Receiver})
	mustAck(t, err)
	if missing != nil || f.gets.Load() != 0 {
		t.Fatal("unmapped channel created a portal or fetched state")
	}
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}, channel: "other"})
	runAckFrames(t, f.client, ackFrame(true))
	if len(f.bot.sent) != 0 {
		t.Fatal("accepted an acknowledgement snapshot from another channel")
	}
	other := f.login(t, "foreign", "beta", false)
	otherKey := networkid.PortalKey{ID: f.key.ID, Receiver: other.UserLogin.ID}
	f.addRoom(t, otherKey, "!foreign:example.test")
	f.addMessage(t, otherKey, "post", "$foreign", "")
	runAckFrames(t, other, ackFrame(true))
	other.channels["channel"] = &mattermost.Channel{ID: "channel", Type: "D"}
	runAckFrames(t, other, ackFrame(true))
	if f.gets.Load() != 1 || len(f.bot.sent) != 0 {
		t.Fatal("foreign team or non-owner DM reached acknowledgement processing")
	}
}

func TestAcknowledgementBackfillAndEdit(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}})
	portal, err := f.bridge.GetExistingPortalByKey(t.Context(), f.key)
	mustAck(t, err)
	response, err := f.client.FetchMessages(t.Context(), bridgev2.FetchMessagesParams{Portal: portal, Forward: true})
	mustAck(t, err)
	if response.CompleteCallback == nil {
		t.Fatal("missing forward reconciliation callback")
	}
	response.CompleteCallback()
	if f.reaction(t, "ack:alice") == nil || f.gets.Load() != 0 {
		t.Fatal("forward backfill ignored fetched acknowledgement metadata")
	}
	response, err = f.client.FetchMessages(t.Context(), bridgev2.FetchMessagesParams{Portal: portal})
	mustAck(t, err)
	r := response.Messages[0].Reactions[0]
	if r.Sender != (bridgev2.EventSender{}) || r.EmojiID != "ack:alice" || r.Emoji != "✅ by Alice Example" {
		t.Fatalf("wrong backfill annotation converter: %+v", r)
	}
	runAckFrames(t, f.client, &mattermost.PostEditedEvent{ChannelID: "channel", Post: ackSnapshot{}.post("")})
	if f.reaction(t, "ack:alice") != nil || f.gets.Load() != 0 {
		t.Fatal("post edit failed to reconcile acknowledgement metadata")
	}
}

func TestReservedAcknowledgementIDs(t *testing.T) {
	f := newAckFixture(t, "")
	f.client.client = mattermost.NewClient("", "")
	reaction := &bridgev2.MatrixReaction{TargetMessage: f.source}
	reaction.Content = &event.ReactionEventContent{RelatesTo: event.RelatesTo{Key: "ack:alice"}}
	if _, err := f.client.PreHandleMatrixReaction(t.Context(), reaction); err == nil {
		t.Fatal("reserved ID accepted by pre-handler")
	}
	reaction.PreHandleResp = &bridgev2.MatrixReactionPreResponse{EmojiID: "ack:alice"}
	if _, err := f.client.HandleMatrixReaction(t.Context(), reaction); err == nil {
		t.Fatal("reserved ID accepted by outbound add")
	}
	remove := &bridgev2.MatrixReactionRemove{TargetReaction: &database.Reaction{MessageID: f.source.ID, EmojiID: "ack:alice"}}
	mustAck(t, f.client.HandleMatrixReactionRemove(t.Context(), remove))
	if f.writes.Load() != 0 {
		t.Fatal("reserved ID reached Mattermost")
	}
	reaction.Content.RelatesTo.Key = "🟢"
	resp, err := f.client.PreHandleMatrixReaction(t.Context(), reaction)
	mustAck(t, err)
	if resp.EmojiID != "green_circle" || strings.HasPrefix(string(resp.EmojiID), ackPrefix) {
		t.Fatalf("ordinary green circle reaction changed: %+v", resp)
	}
	reaction.PreHandleResp = &resp
	_, err = f.client.HandleMatrixReaction(t.Context(), reaction)
	mustAck(t, err)
	remove.TargetReaction.EmojiID = resp.EmojiID
	mustAck(t, f.client.HandleMatrixReactionRemove(t.Context(), remove))
	if f.writes.Load() != 2 {
		t.Fatal("ordinary green circle add/remove did not reach Mattermost")
	}
}

// A reaction named like a reserved status must never enter the ordinary
// reaction path, where it would be mirrored as a user's reaction.
func TestReservedNamedMattermostReactionIsDropped(t *testing.T) {
	f := newAckFixture(t, "")
	var queued []bridgev2.RemoteEvent
	f.client.queueRemoteEventFn = func(e bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, e)
		return bridgev2.EventHandlingResultSuccess
	}
	runAckFrames(t, f.client,
		&mattermost.ReactionEvent{ChannelID: "channel", IsAdd: true, Reaction: &mattermost.Reaction{UserID: "alice", PostID: "post", EmojiName: "ack:alice"}},
		&mattermost.ReactionEvent{ChannelID: "channel", IsAdd: true, Reaction: &mattermost.Reaction{UserID: "alice", PostID: "post", EmojiName: "green_circle"}},
	)
	if len(queued) != 1 {
		t.Fatalf("queued %d reaction events, want only the ordinary one", len(queued))
	}
	if got, ok := queued[0].(*simplevent.Reaction); !ok || got.EmojiID != "green_circle" {
		t.Fatalf("queued the wrong reaction: %#v", queued[0])
	}
}

// Acknowledgement reconciliation reads and then rewrites a post's statuses, so
// it is only correct when every portal event runs one at a time on its lane.
func TestAckQueueSupportedOnlyWithSerializedPortalLane(t *testing.T) {
	for _, tc := range []struct {
		name   string
		buffer int
		async  bool
		want   bool
	}{
		{"serialized lane", 64, false, true},
		{"inline events (no buffer)", 0, false, false},
		{"async events", 64, true, false},
		{"async events without buffer", 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bridge := &bridgev2.Bridge{DB: &database.Database{}, Config: &bridgeconfig.BridgeConfig{PortalEventBuffer: tc.buffer, AsyncEvents: tc.async}}
			c := &MattermostClient{UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}, Bridge: bridge}}
			if got := c.ackQueueSupported(); got != tc.want {
				t.Fatalf("ackQueueSupported() = %t, want %t", got, tc.want)
			}
		})
	}
}

// If the homeserver cannot say whether the source still exists, nothing is
// applied (it could be redacted), and the next pass applies it once it can.
func TestAcknowledgementWaitsWhenSourceCannotBeChecked(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}})
	f.matrixDown.Store(true)
	runAckFrames(t, f.client, ackFrame(true))
	f.reconcile(t)
	if len(f.bot.sent) != 0 || f.reaction(t, "ack:alice") != nil {
		t.Fatalf("applied a status without being able to check the source: %v", f.bot.sent)
	}
	f.matrixDown.Store(false)
	f.reconcile(t)
	if len(f.bot.sent) != 1 || f.reaction(t, "ack:alice") == nil {
		t.Fatal("status was not applied once the source could be checked")
	}
}

// A status whose reaction event was redacted on the homeserver while its bridge
// row survived (the bridge missed the redaction) is replaced, not trusted.
func TestAcknowledgementReplacesStatusRedactedBehindTheBridge(t *testing.T) {
	f := newAckFixture(t, "")
	f.snapshot.Store(ackSnapshot{actors: []string{"alice"}})
	f.reconcile(t)
	row := f.reaction(t, "ack:alice")
	if row == nil || len(f.bot.sent) != 1 {
		t.Fatalf("setup: status was not applied: %v", f.bot.sent)
	}
	f.bot.mu.Lock()
	f.bot.missing[row.MXID] = true // redacted on the homeserver; the bridge row remains
	f.bot.mu.Unlock()
	f.reconcile(t)
	fresh := f.reaction(t, "ack:alice")
	if fresh == nil || fresh.MXID == row.MXID || len(f.bot.sent) != 2 {
		t.Fatalf("redacted status was not replaced: row=%+v sent=%d", fresh, len(f.bot.sent))
	}
}

package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Under time pressure nobody should have to find a particular emoji: any
// reaction opens a burn-on-read placeholder, and any reaction acknowledges a
// post that asks for it. Neither reaches Mattermost as an emoji.
var anyReactions = []string{"🔥", "👍", "❤️", "🎉", "🙏", "ok"}

const (
	ackCall      = "POST /api/v4/users/" + meID + "/posts/p1/ack"
	reactionCall = "POST /api/v4/reactions"
	revealCall   = "GET /api/v4/posts/p1/reveal"
)

func askingPost(author string) *mattermost.Post {
	yes := true
	return &mattermost.Post{
		ID: "p1", UserID: author, ChannelID: "c1", Message: "please confirm",
		Metadata: &mattermost.PostMetadata{Priority: &mattermost.PostPriority{RequestedAck: &yes}},
	}
}

func TestAnyReactionRevealsABurnPlaceholder(t *testing.T) {
	for _, emoji := range anyReactions {
		bs := &burnServer{post: burnPost(otherID, "", true), revealed: revealedBy(otherID, burnExpiry)}
		c, events, _ := burnReactionClient(t, bs)
		r, err := c.HandleMatrixReaction(context.Background(), revealMsg(emoji, &database.Message{ID: "p1", MXID: "$e", SenderID: otherID}))
		if err != nil || r == nil {
			t.Fatalf("%q: HandleMatrixReaction = %v, %v", emoji, r, err)
		}
		if !bs.called(revealCall) || bs.called(reactionCall) || len(*events) != 1 {
			t.Errorf("%q: calls=%v events=%d; want a reveal, no emoji reaction, one edit", emoji, bs.calls, len(*events))
		}
	}
}

func TestAnyReactionAcknowledgesAPostThatAsksForIt(t *testing.T) {
	for _, emoji := range anyReactions {
		bs := &burnServer{post: askingPost(otherID)}
		c, events, _ := burnReactionClient(t, bs)
		r, err := c.HandleMatrixReaction(context.Background(), revealMsg(emoji, &database.Message{ID: "p1", MXID: "$e", SenderID: otherID}))
		if err != nil || r == nil {
			t.Fatalf("%q: HandleMatrixReaction = %v, %v (the reaction must stay in Beeper)", emoji, r, err)
		}
		if !bs.called(ackCall) || bs.called(reactionCall) || bs.called(revealCall) || len(*events) != 0 {
			t.Errorf("%q: calls=%v events=%d; want one acknowledgement and nothing else", emoji, bs.calls, len(*events))
		}
	}
}

func TestReactionIsOrdinaryWhenNothingIsRequested(t *testing.T) {
	yes, no := true, false
	_ = yes
	for name, post := range map[string]*mattermost.Post{
		"plain post":          {ID: "p1", UserID: otherID, Message: "hi"},
		"acknowledgement off": {ID: "p1", UserID: otherID, Message: "hi", Metadata: &mattermost.PostMetadata{Priority: &mattermost.PostPriority{RequestedAck: &no}}},
		"my own request":      askingPost(meID),
		"my own burn post":    burnPost(meID, secretText, true),
	} {
		bs := &burnServer{post: post}
		c, events, _ := burnReactionClient(t, bs)
		if _, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bs.called(reactionCall) || bs.called(ackCall) || bs.called(revealCall) || len(*events) != 0 {
			t.Errorf("%s: calls=%v events=%d; want an ordinary reaction", name, bs.calls, len(*events))
		}
	}
}

func TestReactingAgainDoesNotAcknowledgeTwice(t *testing.T) {
	post := askingPost(otherID)
	post.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: meID, PostID: "p1", AcknowledgedAt: 5}}
	bs := &burnServer{post: post}
	c, _, _ := burnReactionClient(t, bs)
	r, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID}))
	if err != nil || r == nil {
		t.Fatalf("HandleMatrixReaction = %v, %v", r, err)
	}
	if bs.called(ackCall) || bs.called(reactionCall) {
		t.Errorf("calls=%v; a second reaction must neither re-acknowledge (it would restamp the time) nor become an emoji", bs.calls)
	}
}

func TestRefusedAcknowledgementIsReportedAndNotForwarded(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/ack"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"id":"api.acknowledgement.refused","message":"not allowed","status_code":403}`))
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(askingPost(otherID))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	c, _, _ := burnReactionClient(t, &burnServer{post: askingPost(otherID)})
	c.baseURL = srv.URL
	if r, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err == nil || r != nil {
		t.Fatalf("got %v, %v; a refused acknowledgement must surface as an error", r, err)
	}
	for _, call := range calls {
		if call == reactionCall {
			t.Errorf("calls=%v; a refused acknowledgement must not fall back to an emoji reaction", calls)
		}
	}
}

func TestRemovingAnyReactionOnAnActedOnPostIsNotForwarded(t *testing.T) {
	remove := func(emoji string, post *mattermost.Post) (*burnServer, error) {
		bs := &burnServer{post: post}
		c, _, _ := burnReactionClient(t, bs)
		m := &bridgev2.MatrixReactionRemove{}
		m.TargetReaction = &database.Reaction{MessageID: "p1", Emoji: emoji, EmojiID: "thumbsup"}
		return bs, c.HandleMatrixReactionRemove(context.Background(), m)
	}
	const del = "DELETE /api/v4/users/" + meID + "/posts/p1/reactions/thumbsup"
	for _, emoji := range anyReactions {
		if bs, err := remove(emoji, askingPost(otherID)); err != nil || bs.called(del) {
			t.Errorf("%q on a post that asks for acknowledgement: err=%v calls=%v; nothing was added in Mattermost", emoji, err, bs.calls)
		}
		if bs, err := remove(emoji, burnPost(otherID, "", true)); err != nil || bs.called(del) {
			t.Errorf("%q on someone else's burn post: err=%v calls=%v; nothing was added in Mattermost", emoji, err, bs.calls)
		}
	}
	// A reaction Mattermost really holds (made in Mattermost itself) is still removed.
	real := askingPost(otherID)
	real.Metadata.Reactions = []*mattermost.Reaction{{UserID: meID, PostID: "p1", EmojiName: "thumbsup"}}
	if bs, err := remove("👍", real); err != nil || !bs.called(del) {
		t.Errorf("a real reaction held by Mattermost: err=%v calls=%v; it must be removed", err, bs.calls)
	}
	if bs, err := remove("👍", &mattermost.Post{ID: "p1", UserID: otherID, Message: "hi"}); err != nil || !bs.called(del) {
		t.Errorf("ordinary post: err=%v calls=%v; an ordinary reaction must be removed", err, bs.calls)
	}
}

func TestBurnPlaceholderWording(t *testing.T) {
	for name, tc := range map[string]struct {
		read time.Duration
		want string
	}{
		"ten minutes":      {10 * time.Minute, "🔥 Burn-on-read message. Any reaction reveals it. Then it deletes itself 10 minutes later."},
		"one minute":       {time.Minute, "🔥 Burn-on-read message. Any reaction reveals it. Then it deletes itself 1 minute later."},
		"two hours":        {2 * time.Hour, "🔥 Burn-on-read message. Any reaction reveals it. Then it deletes itself 2 hours later."},
		"duration unknown": {0, "🔥 Burn-on-read message. Any reaction reveals it. Then it deletes itself shortly after."},
	} {
		if got := burnPlaceholderBody(tc.read); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.want)
		}
	}
}

// A burn-on-read post that also asks for acknowledgement is opened, not
// acknowledged: the text has to be revealed before there is anything to confirm.
func TestRevealWinsOverAcknowledgementOnTheSamePost(t *testing.T) {
	yes := true
	post := burnPost(otherID, "", true)
	post.Metadata = &mattermost.PostMetadata{Priority: &mattermost.PostPriority{RequestedAck: &yes}}
	bs := &burnServer{post: post, revealed: revealedBy(otherID, burnExpiry)}
	c, _, _ := burnReactionClient(t, bs)
	if _, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err != nil {
		t.Fatal(err)
	}
	if !bs.called(revealCall) || bs.called(ackCall) {
		t.Errorf("calls=%v; want a reveal and no acknowledgement", bs.calls)
	}
}

// If the post cannot be read, nothing may be revealed or acknowledged on a
// guess: the reaction goes through as an emoji, and so does its removal.
func TestUnreadablePostIsTreatedAsAnOrdinaryReaction(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	called := func(call string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range calls {
			if c == call {
				return true
			}
		}
		return false
	}
	c, events, _ := burnReactionClient(t, &burnServer{post: askingPost(otherID)})
	c.baseURL = srv.URL
	if _, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err != nil {
		t.Fatal(err)
	}
	if !called(reactionCall) || called(ackCall) || called(revealCall) || len(*events) != 0 {
		t.Errorf("calls=%v events=%d; an unreadable post must get an ordinary reaction", calls, len(*events))
	}
	m := &bridgev2.MatrixReactionRemove{}
	m.TargetReaction = &database.Reaction{MessageID: "p1", Emoji: "👍", EmojiID: "thumbsup"}
	if err := c.HandleMatrixReactionRemove(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if !called("DELETE /api/v4/users/" + meID + "/posts/p1/reactions/thumbsup") {
		t.Errorf("calls=%v; with the post unreadable the removal is an ordinary one", calls)
	}
}

// --- reveal first, acknowledge afterwards ---------------------------------

// revealedBurnAskingForAck is a burn-on-read post this user has already
// revealed, from an author who also asked for an acknowledgement.
func revealedBurnAskingForAck(withText bool) *mattermost.Post {
	yes := true
	p := burnPost(otherID, "", true)
	p.Metadata = &mattermost.PostMetadata{Priority: &mattermost.PostPriority{RequestedAck: &yes}}
	if withText {
		p.Message = secretText
	} else {
		p.Metadata.ExpireAt = burnExpiry.UnixMilli()
	}
	return p
}

// Reveal and acknowledge are two separate acts on the same message: the first
// reaction opens it, a later one acknowledges it.
func TestAReactionAfterRevealingAcknowledgesInstead(t *testing.T) {
	for name, post := range map[string]*mattermost.Post{
		"revealed (the text came back)": revealedBurnAskingForAck(true),
		"revealed (this user's expiry)": revealedBurnAskingForAck(false),
	} {
		bs := &burnServer{post: post, revealed: revealedBy(otherID, burnExpiry)}
		c, events, _ := burnReactionClient(t, bs)
		r, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID}))
		if err != nil || r == nil {
			t.Fatalf("%s: HandleMatrixReaction = %v, %v", name, r, err)
		}
		if !bs.called(ackCall) || bs.called(revealCall) || bs.called(reactionCall) || len(*events) != 0 {
			t.Errorf("%s: calls=%v events=%d; want one acknowledgement, no second reveal, no emoji", name, bs.calls, len(*events))
		}
	}
}

// A revealed burn post that asks for nothing is an ordinary post to react to.
func TestAReactionAfterRevealingWithNothingRequestedIsOrdinary(t *testing.T) {
	post := burnPost(otherID, secretText, true)
	bs := &burnServer{post: post}
	c, events, _ := burnReactionClient(t, bs)
	if _, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err != nil {
		t.Fatal(err)
	}
	if !bs.called(reactionCall) || bs.called(revealCall) || bs.called(ackCall) || len(*events) != 0 {
		t.Errorf("calls=%v events=%d; want an ordinary reaction", bs.calls, len(*events))
	}
}

// --- the reaction that did the job is taken back --------------------------

func gestureMsg(emoji string, eventID id.EventID) *bridgev2.MatrixReaction {
	m := revealMsg(emoji, &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})
	m.Event = &event.Event{ID: eventID}
	return m
}

type redactions struct {
	mu   sync.Mutex
	list []id.EventID
	err  error
}

func (r *redactions) fn(_ context.Context, _ id.RoomID, eventID id.EventID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, eventID)
	return r.err
}

// Once a reaction has revealed or acknowledged, it is redacted in Beeper:
// otherwise it would look as though that emoji had been sent as a reaction.
func TestAReactionThatRevealedOrAcknowledgedIsRedacted(t *testing.T) {
	for name, post := range map[string]*mattermost.Post{
		"reveal":                   burnPost(otherID, "", true),
		"acknowledge":              askingPost(otherID),
		"acknowledge after reveal": revealedBurnAskingForAck(true),
	} {
		var red redactions
		bs := &burnServer{post: post, revealed: revealedBy(otherID, burnExpiry)}
		c, _, _ := burnReactionClient(t, bs)
		c.redactReactionFn = red.fn
		if _, err := c.HandleMatrixReaction(context.Background(), gestureMsg("🙏", "$reaction1")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(red.list) != 1 || red.list[0] != "$reaction1" {
			t.Errorf("%s: redacted %v, want exactly the user's reaction event", name, red.list)
		}
	}
}

// A second reaction on a post already acknowledged does nothing in Mattermost,
// but it was still only a gesture, so it is taken back too.
func TestAReactionOnAnAlreadyAcknowledgedPostIsRedactedToo(t *testing.T) {
	post := askingPost(otherID)
	post.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: meID, PostID: "p1", AcknowledgedAt: 5}}
	var red redactions
	c, _, _ := burnReactionClient(t, &burnServer{post: post})
	c.redactReactionFn = red.fn
	if _, err := c.HandleMatrixReaction(context.Background(), gestureMsg("👍", "$again")); err != nil {
		t.Fatal(err)
	}
	if len(red.list) != 1 {
		t.Errorf("redacted %v, want the repeat reaction taken back", red.list)
	}
}

// Ordinary reactions stay exactly as the user made them.
func TestOrdinaryReactionsAreNotRedacted(t *testing.T) {
	for name, post := range map[string]*mattermost.Post{
		"ordinary post":    {ID: "p1", UserID: otherID, Message: "hi"},
		"my own request":   askingPost(meID),
		"revealed, no ask": burnPost(otherID, secretText, true),
	} {
		var red redactions
		c, _, _ := burnReactionClient(t, &burnServer{post: post})
		c.redactReactionFn = red.fn
		if _, err := c.HandleMatrixReaction(context.Background(), gestureMsg("👍", "$kept")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(red.list) != 0 {
			t.Errorf("%s: redacted %v; an ordinary reaction must stay", name, red.list)
		}
	}
}

// A refused reveal or acknowledgement keeps the reaction so its failure is
// visible against it; only a success takes it back.
func TestAFailedGestureKeepsItsReaction(t *testing.T) {
	var red redactions
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(askingPost(otherID))
	}))
	defer failing.Close()
	c, _, _ := burnReactionClient(t, &burnServer{post: askingPost(otherID)})
	c.baseURL = failing.URL
	c.redactReactionFn = red.fn
	if _, err := c.HandleMatrixReaction(context.Background(), gestureMsg("👍", "$failed")); err == nil {
		t.Fatal("a refused acknowledgement must fail")
	}
	if len(red.list) != 0 {
		t.Errorf("redacted %v; a failed gesture must keep its reaction", red.list)
	}
}

// The reveal already happened on the server and cannot be undone, so being
// unable to take the reaction back must not turn it into a failure.
func TestACouldNotRedactStillSucceeds(t *testing.T) {
	red := redactions{err: errors.New("no permission")}
	bs := &burnServer{post: burnPost(otherID, "", true), revealed: revealedBy(otherID, burnExpiry)}
	c, events, _ := burnReactionClient(t, bs)
	c.redactReactionFn = red.fn
	r, err := c.HandleMatrixReaction(context.Background(), gestureMsg("🔥", "$stuck"))
	if err != nil || r == nil || len(*events) != 1 {
		t.Fatalf("got %v, %v, %d events; a failed redaction must not undo the reveal", r, err, len(*events))
	}
}

// The framework drops a reaction as a duplicate when the same user already
// holds the same emoji on that message. A gesture must not leave one behind,
// or reacting 🔥 to reveal would make a second 🔥 (to acknowledge) vanish.
func TestAGestureLeavesNothingThatLooksLikeADuplicate(t *testing.T) {
	var ids []string
	for _, event := range []id.EventID{"$one", "$two"} {
		bs := &burnServer{post: askingPost(otherID)}
		c, _, _ := burnReactionClient(t, bs)
		c.redactReactionFn = (&redactions{}).fn
		m := gestureMsg("🔥", event)
		r, err := c.HandleMatrixReaction(context.Background(), m)
		if err != nil || r == nil {
			t.Fatalf("%v, %v", r, err)
		}
		if r.EmojiID == m.PreHandleResp.EmojiID {
			t.Errorf("stored under the real emoji %q: the next reaction with it would be dropped as a duplicate", r.EmojiID)
		}
		ids = append(ids, string(r.EmojiID))
	}
	if ids[0] == ids[1] {
		t.Errorf("two gestures stored under the same id %q", ids[0])
	}
}

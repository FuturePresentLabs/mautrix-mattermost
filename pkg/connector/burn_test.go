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

	"go.mau.fi/util/jsontime"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

const (
	secretText = "SECRET PAYLOAD 7f3a"
	meID       = "me"
	otherID    = "other"
)

var burnExpiry = time.Date(2026, 12, 1, 12, 0, 0, 0, time.UTC)

func burnPost(author, message string, withExpiry bool) *mattermost.Post {
	p := &mattermost.Post{
		ID: "p1", UserID: author, ChannelID: "c1", Type: mattermost.PostTypeBurnOnRead,
		Message: message, CreateAt: burnExpiry.Add(-7 * 24 * time.Hour).UnixMilli(),
		Props: map[string]any{"read_duration": float64(600000)},
	}
	if withExpiry {
		p.Props["expire_at"] = float64(burnExpiry.UnixMilli())
	}
	return p
}

func allText(cm *bridgev2.ConvertedMessage) string {
	var b strings.Builder
	for _, part := range cm.Parts {
		raw, _ := json.Marshal(part.Content)
		b.Write(raw)
	}
	return b.String()
}

func TestConvertBurnOnReadFromSomeoneElseIsAPlaceholder(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	// A recipient's post normally has no text, but the guard must hold even if
	// text, files or a link preview were present.
	post := burnPost(otherID, secretText, true)
	post.FileIDs = []string{"f1"}
	post.Metadata = &mattermost.PostMetadata{
		Files:  []*mattermost.FileInfo{{ID: "f1", Name: "SECRET.png"}},
		Embeds: []*mattermost.PostEmbed{{Type: "opengraph", URL: "https://example.com/SECRET"}},
	}
	cm, err := c.convertPost(context.Background(), nil, "", post)
	if err != nil || cm == nil || len(cm.Parts) != 1 {
		t.Fatalf("convertPost = %+v, %v; want one placeholder part", cm, err)
	}
	if strings.Contains(allText(cm), "SECRET") {
		t.Fatalf("placeholder leaked content: %s", allText(cm))
	}
	body := cm.Parts[0].Content.Body
	if !strings.Contains(body, "🔥") || !strings.Contains(strings.ToLower(body), "burn") || !strings.Contains(body, "10 minutes") {
		t.Errorf("placeholder should explain how to reveal it and the countdown, got %q", body)
	}
	want := database.DisappearingSetting{Type: event.DisappearingTypeAfterSend, DisappearAt: burnExpiry}
	if cm.Disappear.Type != want.Type || !cm.Disappear.DisappearAt.Equal(want.DisappearAt) {
		t.Errorf("Disappear = %+v, want the post's expiry %v", cm.Disappear, burnExpiry)
	}
}

// A placeholder whose post carries no expiry is bounded by the maximum lifetime
// the server reports, carried all the way through conversion.
func TestConvertBurnOnReadPlaceholderWithoutExpiryIsBoundedByTheServersSetting(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	c.setBurnSettings(mattermost.BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute, MaxTTL: 72 * time.Hour})
	post := burnPost(otherID, "", false)
	cm, err := c.convertPost(context.Background(), nil, "", post)
	if err != nil || cm == nil {
		t.Fatal(err)
	}
	bound := post.Timestamp().Add(72 * time.Hour)
	if cm.Disappear.Type != event.DisappearingTypeAfterSend || !cm.Disappear.DisappearAt.Equal(bound) {
		t.Errorf("Disappear = %+v, want after_send at created+72h (%v)", cm.Disappear, bound)
	}
}

// With no maximum reported there is nothing to bound it by, and none is invented.
func TestConvertBurnOnReadPlaceholderWithoutExpiryOrServerSettingHasNoTimer(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	cm, err := c.convertPost(context.Background(), nil, "", burnPost(otherID, "", false))
	if err != nil || cm == nil {
		t.Fatal(err)
	}
	if cm.Disappear != (database.DisappearingSetting{}) {
		t.Errorf("Disappear = %+v, want no timer", cm.Disappear)
	}
}

func TestConvertOwnBurnOnReadShowsTextWithTimer(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	cm, err := c.convertPost(context.Background(), nil, "", burnPost(meID, secretText, true))
	if err != nil || cm == nil || len(cm.Parts) != 1 {
		t.Fatalf("convertPost = %+v, %v", cm, err)
	}
	if !strings.Contains(cm.Parts[0].Content.Body, secretText) {
		t.Errorf("own burn-on-read text should be mirrored, got %q", cm.Parts[0].Content.Body)
	}
	if cm.Disappear.Type != event.DisappearingTypeAfterSend || !cm.Disappear.DisappearAt.Equal(burnExpiry) {
		t.Errorf("Disappear = %+v, want after_send at %v", cm.Disappear, burnExpiry)
	}
}

func TestConvertOwnBurnOnReadWithoutExpiryFailsClosed(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	cm, err := c.convertPost(context.Background(), nil, "", burnPost(meID, secretText, false))
	if err != nil || cm == nil {
		t.Fatal(err)
	}
	if strings.Contains(allText(cm), "SECRET") {
		t.Errorf("without a known expiry the text must not be mirrored: %s", allText(cm))
	}
}

func TestConvertOwnBurnOnReadWithoutTextIsANotice(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	cm, err := c.convertPost(context.Background(), nil, "", burnPost(meID, "", true))
	if err != nil || cm == nil || len(cm.Parts) != 1 {
		t.Fatalf("convertPost = %+v, %v", cm, err)
	}
	if !strings.Contains(strings.ToLower(cm.Parts[0].Content.Body), "you sent a burn-on-read message") {
		t.Errorf("got %q", cm.Parts[0].Content.Body)
	}
}

func TestConvertOwnBurnOnReadDoesNotMirrorFiles(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	post := burnPost(meID, secretText, true)
	post.FileIDs = []string{"f1"}
	// intent is nil, so a mirrored file would be skipped silently: assert on the notice instead.
	cm, _ := c.convertPost(context.Background(), nil, "", post)
	if len(cm.Parts) != 1 || !strings.Contains(strings.ToLower(cm.Parts[0].Content.Body), "attachment") {
		t.Errorf("attachments must be called out, not mirrored: %q", allText(cm))
	}
}

func TestConvertOrdinaryPostIsUnchanged(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	cm, err := c.convertPost(context.Background(), nil, "", &mattermost.Post{ID: "p2", UserID: otherID, Message: "hello"})
	if err != nil || cm == nil || !strings.Contains(cm.Parts[0].Content.Body, "hello") {
		t.Fatalf("ordinary post = %+v, %v", cm, err)
	}
	if cm.Disappear.Type != event.DisappearingTypeNone {
		t.Errorf("ordinary posts must not disappear: %+v", cm.Disappear)
	}
}

func TestBackfillOfBurnOnReadNeverRendersOtherPeoplesText(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	got := c.newBackfillMessage(context.Background(), nil, &bridgev2.Portal{}, burnPost(otherID, secretText, true), "")
	if got == nil {
		t.Fatal("backfill should carry the placeholder, not drop the post")
	}
	if strings.Contains(allText(got.ConvertedMessage), "SECRET") {
		t.Errorf("backfill leaked content: %s", allText(got.ConvertedMessage))
	}
}

func TestPostEditOfBurnOnReadIsRefused(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	edit, err := c.convertPostEdit(context.Background(), nil, "", []*database.Message{{ID: "p1"}}, burnPost(meID, secretText, true))
	if !errors.Is(err, bridgev2.ErrIgnoringRemoteEvent) || edit != nil {
		t.Fatalf("a burn-on-read update must be ignored, got %+v, %v", edit, err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Error("the refusal must not carry content")
	}
	ok, err := c.convertPostEdit(context.Background(), nil, "", []*database.Message{{ID: "p3"}}, &mattermost.Post{ID: "p3", Message: "fixed"})
	if err != nil || len(ok.ModifiedParts) != 1 {
		t.Fatalf("ordinary edits must still work: %+v, %v", ok, err)
	}
}

func mxMsg(content *event.MessageEventContent, portal *bridgev2.Portal) *bridgev2.MatrixMessage {
	m := &bridgev2.MatrixMessage{}
	m.Content, m.Portal = content, portal
	return m
}

func TestBurnRequested(t *testing.T) {
	afterRead := &event.BeeperDisappearingTimer{Type: event.DisappearingTypeAfterRead, Timer: jsontime.MS(10 * time.Minute)}
	afterSend := &event.BeeperDisappearingTimer{Type: event.DisappearingTypeAfterSend, Timer: jsontime.MS(time.Hour)}
	portalWith := func(t event.DisappearingType) *bridgev2.Portal {
		return &bridgev2.Portal{Portal: &database.Portal{Disappear: database.DisappearingSetting{Type: t, Timer: time.Minute}}}
	}
	cases := []struct {
		name string
		msg  *bridgev2.MatrixMessage
		want bool
	}{
		{"message timer after_read", mxMsg(&event.MessageEventContent{BeeperDisappearingTimer: afterRead}, portalWith("")), true},
		{"room timer after_read", mxMsg(&event.MessageEventContent{}, portalWith(event.DisappearingTypeAfterRead)), true},
		{"after_send is not burn-on-read", mxMsg(&event.MessageEventContent{BeeperDisappearingTimer: afterSend}, portalWith(event.DisappearingTypeAfterSend)), false},
		{"no timer", mxMsg(&event.MessageEventContent{}, portalWith("")), false},
		{"explicitly off in the message wins over the room", mxMsg(&event.MessageEventContent{BeeperDisappearingTimer: &event.BeeperDisappearingTimer{Type: event.DisappearingTypeNone}}, portalWith(event.DisappearingTypeAfterRead)), false},
	}
	for _, tc := range cases {
		if got := burnRequested(tc.msg); got != tc.want {
			t.Errorf("%s: burnRequested = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCapabilitiesAdvertiseTheBurnTimerOnlyWhenTheServerAllowsIt(t *testing.T) {
	c := &MattermostClient{}
	if caps := c.GetCapabilities(context.Background(), nil); caps.DisappearingTimer != nil {
		t.Fatal("no timer should be offered before the server's settings are known")
	}
	c.setBurnSettings(mattermost.BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute})
	caps := c.GetCapabilities(context.Background(), nil)
	dt := caps.DisappearingTimer
	if dt == nil || len(dt.Types) != 1 || dt.Types[0] != event.DisappearingTypeAfterRead || len(dt.Timers) != 1 || dt.Timers[0].Duration != 10*time.Minute {
		t.Fatalf("DisappearingTimer = %+v, want one after_read timer of 10m", dt)
	}
	if _, ok := caps.State[event.StateBeeperDisappearingTimer.Type]; !ok {
		t.Error("the disappearing-timer room state must be declared")
	}
	c.setBurnSettings(mattermost.BurnOnReadSettings{})
	if caps := c.GetCapabilities(context.Background(), nil); caps.DisappearingTimer != nil {
		t.Error("a server that turns burn-on-read off must withdraw the timer")
	}
}

func TestHandleMatrixDisappearingTimer(t *testing.T) {
	c := &MattermostClient{}
	c.setBurnSettings(mattermost.BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute})
	mk := func(typ event.DisappearingType, d time.Duration) *bridgev2.MatrixDisappearingTimer {
		m := &bridgev2.MatrixDisappearingTimer{}
		m.Portal = &bridgev2.Portal{Portal: &database.Portal{}}
		m.Content = &event.BeeperDisappearingTimer{Type: typ, Timer: jsontime.MS(d)}
		return m
	}
	ok, err := c.HandleMatrixDisappearingTimer(context.Background(), mk(event.DisappearingTypeAfterRead, 10*time.Minute))
	if !ok || err != nil {
		t.Fatalf("the server's timer must be accepted, got %v, %v", ok, err)
	}
	m := mk(event.DisappearingTypeAfterRead, 10*time.Minute)
	_, _ = c.HandleMatrixDisappearingTimer(context.Background(), m)
	if m.Portal.Disappear.Type != event.DisappearingTypeAfterRead || m.Portal.Disappear.Timer != 10*time.Minute {
		t.Errorf("portal timer = %+v", m.Portal.Disappear)
	}
	if ok, err = c.HandleMatrixDisappearingTimer(context.Background(), mk(event.DisappearingTypeNone, 0)); !ok || err != nil {
		t.Errorf("turning the timer off must work, got %v, %v", ok, err)
	}
	for name, m := range map[string]*bridgev2.MatrixDisappearingTimer{
		"another duration": mk(event.DisappearingTypeAfterRead, time.Hour),
		"after_send":       mk(event.DisappearingTypeAfterSend, 10*time.Minute),
	} {
		if ok, err = c.HandleMatrixDisappearingTimer(context.Background(), m); ok || err == nil {
			t.Errorf("%s must be refused, got %v, %v", name, ok, err)
		}
	}
	c.setBurnSettings(mattermost.BurnOnReadSettings{})
	if ok, err = c.HandleMatrixDisappearingTimer(context.Background(), mk(event.DisappearingTypeAfterRead, 10*time.Minute)); ok || err == nil {
		t.Errorf("a server with burn-on-read off must refuse the timer, got %v, %v", ok, err)
	}
}

// burnServer is a minimal Mattermost for the REST calls burn-on-read makes.
type burnServer struct {
	mu    sync.Mutex
	calls []string
	post  *mattermost.Post
	// revealed is returned by /reveal.
	revealed *mattermost.Post
	status   int
}

func (b *burnServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls = append(b.calls, r.Method+" "+r.URL.Path)
		b.mu.Unlock()
		if b.status != 0 {
			w.WriteHeader(b.status)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/reveal"):
			_ = json.NewEncoder(w).Encode(b.revealed)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/posts/"):
			_ = json.NewEncoder(w).Encode(b.post)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (b *burnServer) called(call string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.calls {
		if c == call {
			return true
		}
	}
	return false
}

func TestRemovingABurnOnReadMessageBurnsItInsteadOfDeleting(t *testing.T) {
	bs := &burnServer{post: burnPost(otherID, "", true)}
	srv := bs.start(t)
	c := &MattermostClient{UserID: meID, Token: "tok", baseURL: srv.URL}
	if err := c.removePost("p1"); err != nil {
		t.Fatal(err)
	}
	if !bs.called("DELETE /api/v4/posts/p1/burn") || bs.called("DELETE /api/v4/posts/p1") {
		t.Errorf("calls = %v, want burn and no delete", bs.calls)
	}
}

func TestRemovingAnOrdinaryMessageStillDeletes(t *testing.T) {
	bs := &burnServer{post: &mattermost.Post{ID: "p2", UserID: meID, Message: "hi"}}
	srv := bs.start(t)
	c := &MattermostClient{UserID: meID, Token: "tok", baseURL: srv.URL}
	if err := c.removePost("p2"); err != nil {
		t.Fatal(err)
	}
	if !bs.called("DELETE /api/v4/posts/p2") || bs.called("DELETE /api/v4/posts/p2/burn") {
		t.Errorf("calls = %v", bs.calls)
	}
}

func TestRevealBurnOnReadReturnsTextAndExpiry(t *testing.T) {
	revealed := burnPost(otherID, secretText, true)
	revealed.Metadata = &mattermost.PostMetadata{ExpireAt: burnExpiry.Add(-6 * 24 * time.Hour).UnixMilli()}
	bs := &burnServer{post: burnPost(otherID, "", true), revealed: revealed}
	srv := bs.start(t)
	c := &MattermostClient{UserID: meID, Token: "tok", baseURL: srv.URL}
	got, err := c.revealBurnOnRead(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Content == nil || !strings.Contains(got.Content.Body, secretText) {
		t.Errorf("revealed content = %+v", got.Content)
	}
	if !got.ExpireAt.Equal(burnExpiry.Add(-6 * 24 * time.Hour)) {
		t.Errorf("ExpireAt = %v", got.ExpireAt)
	}
	if !bs.called("GET /api/v4/posts/p1/reveal") {
		t.Errorf("calls = %v", bs.calls)
	}
}

func TestRevealRefusesNonBurnOnReadAndOwnPosts(t *testing.T) {
	for name, post := range map[string]*mattermost.Post{
		"ordinary post": {ID: "p1", UserID: otherID, Message: "hi"},
		"own post":      burnPost(meID, secretText, true),
	} {
		bs := &burnServer{post: post}
		srv := bs.start(t)
		c := &MattermostClient{UserID: meID, Token: "tok", baseURL: srv.URL}
		if kind, _ := c.classifyReaction(context.Background(), "p1"); kind != reactionOrdinary {
			t.Errorf("%s: classified as %v; the reaction must pass through untouched", name, kind)
		}
		if bs.called("GET /api/v4/posts/p1/reveal") {
			t.Errorf("%s: reveal must not be called", name)
		}
	}
}

func TestRevealFailureIsReportedNotSwallowed(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reveal") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(burnPost(otherID, "", true))
	}))
	defer failing.Close()
	c, events, _ := burnReactionClient(t, &burnServer{post: burnPost(otherID, "", true)})
	c.baseURL = failing.URL
	r, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID}))
	if err == nil || r != nil || len(*events) != 0 {
		t.Errorf("got %v, %v, %d events; a refused reveal must surface as an error and show nothing", r, err, len(*events))
	}
}

func TestBurnedPostRemovesItsMessage(t *testing.T) {
	var got []bridgev2.RemoteEvent
	c := &MattermostClient{UserID: meID, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}, queueRemoteEventFn: func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		got = append(got, ev)
		return bridgev2.EventHandlingResultSuccess
	}}
	if !c.handleBurnLifecycle(context.Background(), &mattermost.PostBurnedEvent{PostID: "p1", ChannelID: "c1"}) {
		t.Fatal("a burned event must be handled")
	}
	if len(got) != 1 || got[0].GetType() != bridgev2.RemoteEventMessageRemove {
		t.Fatalf("queued %v, want one message removal", got)
	}
	if rm, ok := got[0].(*simplevent.MessageRemove); !ok || rm.TargetMessage != "p1" {
		t.Errorf("removal target = %+v", got[0])
	}
}

func TestRevealedAndAllRevealedMoveTheDisappearTime(t *testing.T) {
	type call struct {
		channel, post string
		at            time.Time
	}
	var calls []call
	c := &MattermostClient{UserID: meID}
	c.scheduleDisappearFn = func(ctx context.Context, channelID, postID string, at time.Time) error {
		calls = append(calls, call{channelID, postID, at})
		return nil
	}
	at := time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC)
	// Someone else's post revealed on another device: the user's countdown ends then.
	c.handleBurnLifecycle(context.Background(), &mattermost.PostRevealedEvent{PostID: "p1", ChannelID: "c1", AuthorID: otherID, ExpireAt: at.UnixMilli()})
	// Our own post, all recipients revealed: the sender's copy ends then.
	c.handleBurnLifecycle(context.Background(), &mattermost.PostAllRevealedEvent{PostID: "p2", ChannelID: "c1", SenderExpireAt: at.UnixMilli()})
	// The author's own recipient-count update is not a reveal by this user.
	c.handleBurnLifecycle(context.Background(), &mattermost.PostRevealedEvent{PostID: "p3", ChannelID: "c1", AuthorID: meID, ExpireAt: at.UnixMilli()})
	// No usable time: nothing to schedule.
	c.handleBurnLifecycle(context.Background(), &mattermost.PostRevealedEvent{PostID: "p4", ChannelID: "c1", AuthorID: otherID})
	want := []call{{"c1", "p1", at}, {"c1", "p2", at}}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	for i := range want {
		if calls[i].channel != want[i].channel || calls[i].post != want[i].post || !calls[i].at.Equal(want[i].at) {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

func TestOtherEventsAreNotBurnLifecycle(t *testing.T) {
	c := &MattermostClient{}
	if c.handleBurnLifecycle(context.Background(), &mattermost.TypingEvent{}) {
		t.Error("a typing event is not a burn-on-read event")
	}
}

// --- reactions, edits and the reveal's failure modes ----------------------

func revealMsg(emoji string, target *database.Message) *bridgev2.MatrixReaction {
	m := &bridgev2.MatrixReaction{}
	m.Content = &event.ReactionEventContent{RelatesTo: event.RelatesTo{Key: emoji}}
	m.Portal = &bridgev2.Portal{Portal: &database.Portal{MXID: "!room:example.org"}}
	m.TargetMessage = target
	m.PreHandleResp = &bridgev2.MatrixReactionPreResponse{EmojiID: "fire", Emoji: emoji}
	return m
}

func burnReactionClient(t *testing.T, bs *burnServer) (*MattermostClient, *[]bridgev2.RemoteEvent, *[]time.Time) {
	srv := bs.start(t)
	var events []bridgev2.RemoteEvent
	var scheduled []time.Time
	c := &MattermostClient{UserID: meID, Token: "tok", baseURL: srv.URL, client: mattermost.NewClient("http://example.invalid", "tok")}
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		events = append(events, ev)
		return bridgev2.EventHandlingResultSuccess
	}
	c.addDisappearFn = func(ctx context.Context, roomID id.RoomID, eventID id.EventID, at time.Time) error {
		scheduled = append(scheduled, at)
		return nil
	}
	return c, &events, &scheduled
}

func revealedBy(author string, expire time.Time) *mattermost.Post {
	p := burnPost(author, secretText, true)
	if !expire.IsZero() {
		p.Metadata = &mattermost.PostMetadata{ExpireAt: expire.UnixMilli()}
	}
	return p
}

func TestRevealReactionRevealsAndIsNotSentAsAReaction(t *testing.T) {
	expire := time.Now().Add(10 * time.Minute).Truncate(time.Millisecond)
	bs := &burnServer{post: burnPost(otherID, "", true), revealed: revealedBy(otherID, expire)}
	c, events, scheduled := burnReactionClient(t, bs)
	r, err := c.HandleMatrixReaction(context.Background(), revealMsg("🔥", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID}))
	if err != nil || r == nil {
		t.Fatalf("HandleMatrixReaction = %v, %v", r, err)
	}
	if !bs.called("GET /api/v4/posts/p1/reveal") || bs.called("POST /api/v4/reactions") {
		t.Errorf("calls = %v; want a reveal and no reaction", bs.calls)
	}
	if len(*events) != 1 || (*events)[0].GetType() != bridgev2.RemoteEventEdit {
		t.Fatalf("queued %v, want one edit", *events)
	}
	if len(*scheduled) != 1 || !(*scheduled)[0].Equal(expire) {
		t.Errorf("disappear scheduled at %v, want %v", *scheduled, expire)
	}
	edit, err := (*events)[0].(*simplevent.Message[*revealedPost]).ConvertEditFunc(context.Background(), nil, nil, []*database.Message{{ID: "p1"}}, (*events)[0].(*simplevent.Message[*revealedPost]).Data)
	if err != nil || len(edit.ModifiedParts) != 1 || !strings.Contains(edit.ModifiedParts[0].Content.Body, secretText) {
		t.Errorf("the edit must carry the revealed text, got %+v, %v", edit, err)
	}
	meta, ok := edit.ModifiedParts[0].Part.Metadata.(*MessageMetadata)
	if !ok || !meta.AckPromptTracked || !meta.AckPromptUnsafe {
		t.Errorf("revealed metadata = %#v, want tracked and unsafe", edit.ModifiedParts[0].Part.Metadata)
	}
}

func TestRevealIsScheduledBeforeTheTextIsSentAndFailsClosed(t *testing.T) {
	bs := &burnServer{post: burnPost(otherID, "", true), revealed: revealedBy(otherID, time.Now().Add(10*time.Minute))}
	c, events, _ := burnReactionClient(t, bs)
	c.addDisappearFn = func(context.Context, id.RoomID, id.EventID, time.Time) error { return errors.New("db down") }
	r, err := c.HandleMatrixReaction(context.Background(), revealMsg("🔥", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID}))
	if err == nil || r != nil {
		t.Fatalf("got %v, %v; a reveal that cannot be bounded must fail", r, err)
	}
	if len(*events) != 0 {
		t.Errorf("no text may be sent when its removal cannot be scheduled, queued %v", *events)
	}
}

func TestRevealWithoutAnExpiryUsesTheServersBurnDuration(t *testing.T) {
	bs := &burnServer{post: burnPost(otherID, "", true), revealed: revealedBy(otherID, time.Time{})}
	c, _, scheduled := burnReactionClient(t, bs)
	c.setBurnSettings(mattermost.BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute})
	before := time.Now()
	if _, err := c.HandleMatrixReaction(context.Background(), revealMsg("🔥", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err != nil {
		t.Fatal(err)
	}
	if len(*scheduled) != 1 || (*scheduled)[0].Before(before.Add(9*time.Minute)) || (*scheduled)[0].After(before.Add(11*time.Minute)) {
		t.Errorf("scheduled %v, want about 10 minutes from now", *scheduled)
	}
	c2, events2, _ := burnReactionClient(t, &burnServer{post: burnPost(otherID, "", true), revealed: revealedBy(otherID, time.Time{})})
	if _, err := c2.HandleMatrixReaction(context.Background(), revealMsg("🔥", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err == nil || len(*events2) != 0 {
		t.Errorf("with no expiry from either source nothing may be shown, err=%v events=%v", err, *events2)
	}
}

func TestOrdinaryReactionsStillGoToMattermost(t *testing.T) {
	for name, post := range map[string]*mattermost.Post{
		"fire on an ordinary post": {ID: "p1", UserID: otherID, Message: "hi"},
		"fire on my own burn post": burnPost(meID, secretText, true),
	} {
		bs := &burnServer{post: post}
		c, events, _ := burnReactionClient(t, bs)
		if _, err := c.HandleMatrixReaction(context.Background(), revealMsg("🔥", &database.Message{ID: "p1", MXID: "$e", SenderID: otherID})); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bs.called("POST /api/v4/reactions") || bs.called("GET /api/v4/posts/p1/reveal") || len(*events) != 0 {
			t.Errorf("%s: calls=%v events=%v", name, bs.calls, *events)
		}
	}
	// Any reaction can be a reveal or an acknowledgement, so every reaction
	// looks the post up once; an ordinary one is then sent as an emoji.
	bs := &burnServer{post: &mattermost.Post{ID: "p1", UserID: otherID, Message: "hi"}}
	c, _, _ := burnReactionClient(t, bs)
	if _, err := c.HandleMatrixReaction(context.Background(), revealMsg("👍", &database.Message{ID: "p1", MXID: "$e"})); err != nil {
		t.Fatal(err)
	}
	if !bs.called("GET /api/v4/posts/p1") || !bs.called("POST /api/v4/reactions") {
		t.Errorf("calls=%v; want one lookup and then an ordinary reaction", bs.calls)
	}
}

func TestRemovingFireIsSkippedOnlyOnSomeoneElsesBurnPost(t *testing.T) {
	rm := func(post *mattermost.Post) (*burnServer, error) {
		bs := &burnServer{post: post}
		c, _, _ := burnReactionClient(t, bs)
		m := &bridgev2.MatrixReactionRemove{}
		m.TargetReaction = &database.Reaction{MessageID: "p1", Emoji: "🔥", EmojiID: "fire"}
		return bs, c.HandleMatrixReactionRemove(context.Background(), m)
	}
	const del = "DELETE /api/v4/users/me/posts/p1/reactions/fire"
	if bs, err := rm(burnPost(otherID, "", true)); err != nil || bs.called(del) {
		t.Errorf("someone else's burn post: err=%v calls=%v; the reveal left no reaction to remove", err, bs.calls)
	}
	if bs, err := rm(burnPost(meID, secretText, true)); err != nil || !bs.called(del) {
		t.Errorf("my own burn post: err=%v calls=%v; a real fire reaction must be removed", err, bs.calls)
	}
	if bs, err := rm(&mattermost.Post{ID: "p1", UserID: otherID}); err != nil || !bs.called(del) {
		t.Errorf("ordinary post: err=%v calls=%v", err, bs.calls)
	}
}

func TestEditingABurnOnReadMessageIsRefusedOutbound(t *testing.T) {
	bs := &burnServer{post: burnPost(meID, secretText, true)}
	c, _, _ := burnReactionClient(t, bs)
	m := &bridgev2.MatrixEdit{EditTarget: &database.Message{ID: "p1"}}
	m.Content = &event.MessageEventContent{Body: "changed"}
	if err := c.HandleMatrixEdit(context.Background(), m); err == nil {
		t.Fatal("an edit of a burn-on-read message must be refused")
	}
	if bs.called("PUT /api/v4/posts/p1/patch") || bs.called("PUT /api/v4/posts/p1") {
		t.Errorf("no edit may reach Mattermost: %v", bs.calls)
	}
}

func TestBurnedEventWithoutAChannelIsDropped(t *testing.T) {
	var got []bridgev2.RemoteEvent
	c := &MattermostClient{UserID: meID, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}, queueRemoteEventFn: func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		got = append(got, ev)
		return bridgev2.EventHandlingResultSuccess
	}}
	c.handleBurnLifecycle(context.Background(), &mattermost.PostBurnedEvent{PostID: "p1"})
	if len(got) != 0 {
		t.Errorf("an event with no channel must not queue a removal for an empty portal: %v", got)
	}
}

func TestEphemeralBurnOnReadPostIsNotRelayed(t *testing.T) {
	var got []bridgev2.RemoteEvent
	c := &MattermostClient{UserID: meID, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}, queueRemoteEventFn: func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		got = append(got, ev)
		return bridgev2.EventHandlingResultSuccess
	}}
	c.relayEphemeral(burnPost(otherID, secretText, true))
	if len(got) != 0 {
		t.Errorf("a burn-on-read post must never be relayed as a notice: %v", got)
	}
}

func TestBurnDisappearNeverHasANegativeTimer(t *testing.T) {
	post := burnPost(otherID, "", true)
	post.CreateAt = burnExpiry.Add(time.Hour).UnixMilli() // expiry before creation, e.g. clock skew
	if d := (&MattermostClient{}).burnDisappear(post); d.Timer < 0 {
		t.Errorf("Timer = %v, want >= 0", d.Timer)
	}
}

func TestBurnTimerFeaturesKeepExistingState(t *testing.T) {
	c := &MattermostClient{}
	c.setBurnSettings(mattermost.BurnOnReadSettings{Enabled: true, Duration: time.Minute})
	f := &event.RoomFeatures{State: event.StateFeatureMap{event.StateRoomName.Type: {Level: event.CapLevelFullySupported}}}
	c.burnTimerFeatures(f)
	if _, ok := f.State[event.StateRoomName.Type]; !ok {
		t.Error("existing state features must survive")
	}
	if _, ok := f.State[event.StateBeeperDisappearingTimer.Type]; !ok {
		t.Error("the timer state must be added")
	}
}

func TestConversionOfBurnOnReadMakesNoRequests(t *testing.T) {
	bs := &burnServer{}
	srv := bs.start(t)
	c := &MattermostClient{UserID: meID, Token: "tok", baseURL: srv.URL}
	own := burnPost(meID, secretText, true)
	own.FileIDs = []string{"f1"}
	theirs := burnPost(otherID, secretText, true)
	theirs.FileIDs = []string{"f1"}
	for _, p := range []*mattermost.Post{own, theirs} {
		if _, err := c.convertPost(context.Background(), nil, "", p); err != nil {
			t.Fatal(err)
		}
	}
	if len(bs.calls) != 0 {
		t.Errorf("converting a burn-on-read post must not fetch files or previews, calls = %v", bs.calls)
	}
}

func TestBurnSettingsAreRefreshedWhileConnected(t *testing.T) {
	var mu sync.Mutex
	enabled := "true"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write([]byte(`{"EnableBurnOnRead":"` + enabled + `","BurnOnReadDurationSeconds":"600"}`))
	}))
	defer srv.Close()
	c := &MattermostClient{Token: "tok", baseURL: srv.URL}
	done := make(chan struct{})
	defer close(done)
	go c.runBurnSettingsLoop(done, 20*time.Millisecond)
	waitFor := func(want bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if c.burnSettings().Enabled == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("settings never reached Enabled=%v", want)
	}
	waitFor(true)
	mu.Lock()
	enabled = "false"
	mu.Unlock()
	waitFor(false)
}

func TestBackfilledOwnBurnPostCarriesItsRemovalTime(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	for name, p := range map[string]*mattermost.Post{
		"own":    burnPost(meID, secretText, true),
		"theirs": burnPost(otherID, "", true),
	} {
		m := c.newBackfillMessage(context.Background(), nil, &bridgev2.Portal{Portal: &database.Portal{}}, p, "")
		if m.Disappear.Type == event.DisappearingTypeNone || m.Disappear.DisappearAt.IsZero() {
			t.Errorf("%s: backfilled message has no removal time: %+v", name, m.Disappear)
		}
	}
}

// --- removal-time reconciliation ------------------------------------------

type removalFixture struct {
	c        *MattermostClient
	added    []id.EventID
	present  map[id.EventID]bool // events with a stored removal time
	messages map[string]id.EventID
}

func newRemovalFixture() *removalFixture {
	f := &removalFixture{present: map[id.EventID]bool{}, messages: map[string]id.EventID{}}
	f.c = &MattermostClient{
		burnLookupFn: func(ctx context.Context, postID string) (id.RoomID, id.EventID, bool, error) {
			ev, ok := f.messages[postID]
			return "!room:example.org", ev, ok, nil
		},
		hasDisappearFn: func(ctx context.Context, ev id.EventID) (bool, error) { return f.present[ev], nil },
		addDisappearFn: func(ctx context.Context, room id.RoomID, ev id.EventID, at time.Time) error {
			f.added = append(f.added, ev)
			f.present[ev] = true
			return nil
		},
	}
	return f
}

func TestReconcileAddsAMissingRemovalTimeOnce(t *testing.T) {
	f := newRemovalFixture()
	at := time.Now().Add(time.Hour)
	f.c.trackBurnRemoval("p1", at)
	f.c.reconcileBurnRemovals(context.Background()) // not stored yet: keep waiting
	if len(f.added) != 0 {
		t.Fatalf("nothing to repair before the message is stored: %v", f.added)
	}
	f.messages["p1"] = "$e1"
	f.c.reconcileBurnRemovals(context.Background())
	f.c.reconcileBurnRemovals(context.Background())
	if len(f.added) != 1 || f.added[0] != "$e1" {
		t.Errorf("added = %v, want exactly one for $e1", f.added)
	}
}

func TestReconcileLeavesAnExistingRemovalTimeAlone(t *testing.T) {
	f := newRemovalFixture()
	f.messages["p1"] = "$e1"
	f.present["$e1"] = true
	f.c.trackBurnRemoval("p1", time.Now().Add(time.Hour))
	f.c.reconcileBurnRemovals(context.Background())
	if len(f.added) != 0 {
		t.Errorf("a stored removal time must not be duplicated: %v", f.added)
	}
}

func TestReconcileGivesUpOnAMessageThatNeverAppears(t *testing.T) {
	f := newRemovalFixture()
	f.c.trackBurnRemoval("p1", time.Now().Add(-2*time.Hour))
	f.c.reconcileBurnRemovals(context.Background())
	f.messages["p1"] = "$e1"
	f.c.reconcileBurnRemovals(context.Background())
	if len(f.added) != 0 {
		t.Errorf("an entry long past its removal time is dropped: %v", f.added)
	}
}

func TestBackfillTracksEveryBurnPostItConverts(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	c.newBackfillMessage(context.Background(), nil, &bridgev2.Portal{Portal: &database.Portal{}}, burnPost(meID, secretText, true), "")
	c.newBackfillMessage(context.Background(), nil, &bridgev2.Portal{Portal: &database.Portal{}}, &mattermost.Post{ID: "plain", UserID: meID, Message: "hi"}, "")
	if n := c.pendingBurnRemovals(); n != 1 {
		t.Errorf("tracking %d posts, want 1 (the burn-on-read one)", n)
	}
}

// The longest a burn-on-read post may exist is a server setting, so the bound on
// a placeholder follows it. Nothing here may assume the stock seven days.
func TestBurnDisappearUsesTheServersMaximumLifetime(t *testing.T) {
	for name, ttl := range map[string]time.Duration{"three days": 72 * time.Hour, "one day": 24 * time.Hour, "thirty days": 30 * 24 * time.Hour} {
		post := burnPost(otherID, "", false) // the post carries no expiry of its own
		c := &MattermostClient{}
		c.setBurnSettings(mattermost.BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute, MaxTTL: ttl})
		d := c.burnDisappear(post)
		if d.Timer != ttl || !d.DisappearAt.Equal(post.Timestamp().Add(ttl)) {
			t.Errorf("%s: timer=%v at=%v, want %v after the post was sent", name, d.Timer, d.DisappearAt, ttl)
		}
	}
}

// If the server has not reported a maximum, no bound is invented: the placeholder
// holds no text, and burn lifecycle events remove it.
func TestBurnDisappearInventsNoBoundWhenTheServerHasNotSaid(t *testing.T) {
	post := burnPost(otherID, "", false)
	for name, settings := range map[string]mattermost.BurnOnReadSettings{
		"settings never read":      {},
		"enabled, no maximum sent": {Enabled: true, Duration: 10 * time.Minute},
	} {
		c := &MattermostClient{}
		c.setBurnSettings(settings)
		if d := c.burnDisappear(post); d != (database.DisappearingSetting{}) {
			t.Errorf("%s: got %+v, want no timer", name, d)
		}
	}
}

// A post's own expiry is exact, so it beats the general maximum.
func TestBurnDisappearPrefersThePostsOwnExpiry(t *testing.T) {
	post := burnPost(otherID, "", true)
	c := &MattermostClient{}
	c.setBurnSettings(mattermost.BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute, MaxTTL: time.Hour})
	d := c.burnDisappear(post)
	if want := burnExpiry.Sub(post.Timestamp()); d.Timer != want || !d.DisappearAt.Equal(burnExpiry) {
		t.Errorf("timer=%v at=%v, want %v ending at the post's own expiry", d.Timer, d.DisappearAt, want)
	}
}

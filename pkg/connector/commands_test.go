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
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

func TestCommandTrigger(t *testing.T) {
	cases := map[string]string{
		"/away":            "away",
		"/Meet start":      "meet",
		"/gcal\nlist":      "gcal",
		"/google-drive x":  "google-drive",
		"/usr/bin is full": "",
		"hello /away":      "",
		"/":                "",
		" /away":           "",
	}
	for body, want := range cases {
		got, ok := commandTrigger(strings.ReplaceAll(body, "\\n", "\n"))
		if got != want || ok != (want != "") {
			t.Errorf("%q: got %q %v, want %q", body, got, ok, want)
		}
	}
}

// commandServer is a Mattermost server with /away and /meet in team tA.
type commandServer struct {
	mu        sync.Mutex
	lists     int
	executed  []map[string]any
	posted    []string
	executeFn func(w http.ResponseWriter, body map[string]any)
}

func (s *commandServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/api/v4/teams/tA/commands/autocomplete":
			s.lists++
			_, _ = w.Write([]byte(`[{"trigger":"away"},{"trigger":"Meet"}]`))
		case "/api/v4/commands":
			w.WriteHeader(http.StatusForbidden)
		case "/api/v4/commands/execute":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.executed = append(s.executed, body)
			if s.executeFn != nil {
				s.executeFn(w, body)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		case "/api/v4/posts":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.posted = append(s.posted, body["message"].(string))
			_, _ = w.Write([]byte(`{"id":"post1"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func commandClient(t *testing.T, s *commandServer) (*MattermostClient, *[]bridgev2.RemoteEvent) {
	srv := s.start(t)
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	c.ServerURL = srv.URL
	c.client = &mattermost.Client{}
	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return bridgev2.EventHandlingResultQueued
	}
	return c, &queued
}

func send(t *testing.T, c *MattermostClient, body string) (*bridgev2.MatrixMessageResponse, error) {
	msg := makeMatrixMessage("ch1", event.MsgText, body)
	msg.Event = &event.Event{ID: id.EventID("$evt")}
	return c.HandleMatrixMessage(context.Background(), msg)
}

func TestKnownCommandRunsInsteadOfPosting(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	resp, err := send(t, c, "/meet start")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.posted) != 0 || len(s.executed) != 1 {
		t.Fatalf("posted %v executed %v", s.posted, s.executed)
	}
	if got := s.executed[0]; got["command"] != "/meet start" || got["channel_id"] != "ch1" || got["team_id"] != "tA" {
		t.Fatalf("execute body = %v", got)
	}
	if resp.DB.ID != "cmd:$evt" || !isCommandMessage(resp.DB.ID) {
		t.Fatalf("saved as %q", resp.DB.ID)
	}
}

func TestNonCommandsArePosted(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	for _, body := range []string{"/usr/bin is full", "/nope hello", "hello", "//away is a command"} {
		if _, err := send(t, c, body); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.executed) != 0 {
		t.Fatalf("executed %v", s.executed)
	}
	if strings.Join(s.posted, "|") != "/usr/bin is full|/nope hello|hello|/away is a command" {
		t.Fatalf("posted %q", s.posted)
	}
}

func TestOnlyTextRunsCommands(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	msg := makeMatrixMessage("ch1", event.MsgNotice, "/away")
	msg.Event = &event.Event{ID: "$n"}
	if _, err := c.HandleMatrixMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if len(s.executed) != 0 || len(s.posted) != 1 {
		t.Fatalf("notice: executed %v posted %v", s.executed, s.posted)
	}
}

func TestCommandInThread(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	msg := makeMatrixMessage("ch1", event.MsgText, "/away")
	msg.Event = &event.Event{ID: "$t"}
	msg.ThreadRoot = &database.Message{ID: "rootpost"}
	if _, err := c.HandleMatrixMessage(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if s.executed[0]["root_id"] != "rootpost" {
		t.Fatalf("root = %v", s.executed[0]["root_id"])
	}
}

func TestCommandErrorShowsMattermostMessage(t *testing.T) {
	s := &commandServer{executeFn: func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"You do not have permission"}`))
	}}
	c, _ := commandClient(t, s)
	_, err := send(t, c, "/away")
	if err == nil || !strings.Contains(err.Error(), "/away: You do not have permission") {
		t.Fatalf("err = %v", err)
	}
	if len(s.posted) != 0 {
		t.Fatal("failed command was posted as text")
	}
}

func TestGotoLocationNotice(t *testing.T) {
	s := &commandServer{executeFn: func(w http.ResponseWriter, _ map[string]any) {
		_, _ = w.Write([]byte(`{"goto_location":"https://mm.example.com/team/messages/@alice"}`))
	}}
	c, queued := commandClient(t, s)
	if _, err := send(t, c, "/away"); err != nil {
		t.Fatal(err)
	}
	if len(*queued) != 1 || !strings.Contains(noticeText(t, (*queued)[0]), "https://mm.example.com/team/messages/@alice") {
		t.Fatalf("queued %v", *queued)
	}
}

func TestCommandListCachedAndRefreshedOnMiss(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	ctx := context.Background()
	if !c.knownCommand(ctx, "tA", "away") || !c.knownCommand(ctx, "tA", "meet") || s.lists != 1 {
		t.Fatalf("lists = %d", s.lists)
	}
	if c.knownCommand(ctx, "tA", "newplugin") || s.lists != 1 {
		t.Fatalf("fresh list refetched on a miss: %d", s.lists)
	}
	c.commands["tA"].fetchedAt = time.Now().Add(-2 * time.Minute)
	c.knownCommand(ctx, "tA", "away")
	if s.lists != 1 {
		t.Fatal("hit refetched")
	}
	c.knownCommand(ctx, "tA", "newplugin")
	if s.lists != 2 {
		t.Fatalf("stale miss not refetched: %d", s.lists)
	}
	c.commands["tA"].fetchedAt = time.Now().Add(-commandListTTL - time.Second)
	c.knownCommand(ctx, "tA", "away")
	if s.lists != 3 {
		t.Fatalf("expired list not refetched: %d", s.lists)
	}
}

func TestCommandListErrorKeepsCache(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	c.commands = map[string]*commandCache{"tA": {triggers: map[string]bool{"away": true}, fetchedAt: time.Now().Add(-time.Hour)}}
	c.listCommandsFn = func(string) ([]mattermost.Command, error) { return nil, context.DeadlineExceeded }
	if !c.knownCommand(context.Background(), "tA", "away") || c.knownCommand(context.Background(), "tA", "meet") {
		t.Fatal("cache not used when the list fails")
	}
}

func TestCommandMessageGuards(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	c.client = &mattermost.Client{}
	c.ServerURL = "http://127.0.0.1:1"
	cmd := &database.Message{ID: "cmd:$e"}
	if err := c.HandleMatrixEdit(context.Background(), &bridgev2.MatrixEdit{EditTarget: cmd, MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Content: &event.MessageEventContent{Body: "x"}}}); !errors.Is(err, errCommandMessage) {
		t.Fatal("edit of a command allowed")
	}
	if err := c.HandleMatrixMessageRemove(context.Background(), &bridgev2.MatrixMessageRemove{TargetMessage: cmd}); err != nil {
		t.Fatalf("delete of a command: %v", err)
	}
	if _, err := c.PreHandleMatrixReaction(context.Background(), &bridgev2.MatrixReaction{TargetMessage: cmd, MatrixEventBase: bridgev2.MatrixEventBase[*event.ReactionEventContent]{Content: &event.ReactionEventContent{}}}); err == nil {
		t.Fatal("reaction to a command allowed")
	}
	reply := makeMatrixMessage("ch1", event.MsgText, "x")
	reply.ReplyTo = cmd
	if got := outboundRootID(reply); got != "" {
		t.Fatalf("reply root = %q", got)
	}
	reply.ReplyTo = &database.Message{ID: "cmd:$f", ThreadRoot: "realroot"}
	if got := outboundRootID(reply); got != "realroot" {
		t.Fatalf("command in a thread: root = %q", got)
	}
}

func noticeText(t *testing.T, ev bridgev2.RemoteEvent) string {
	t.Helper()
	m, ok := ev.(*simplevent.Message[string])
	if !ok {
		t.Fatalf("not a notice: %T", ev)
	}
	conv, err := m.ConvertMessageFunc(context.Background(), nil, nil, m.Data)
	if err != nil {
		t.Fatal(err)
	}
	if conv.Parts[0].Content.MsgType != event.MsgNotice {
		t.Fatalf("msgtype = %s", conv.Parts[0].Content.MsgType)
	}
	return conv.Parts[0].Content.Body
}

func TestEphemeralRepliesBecomeNotices(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return bridgev2.EventHandlingResultQueued
	}
	c.relayEphemeral(&mattermost.Post{ChannelID: "ch1", RootID: "root1", Message: "You are now **away**",
		Props: map[string]any{"attachments": []any{map[string]any{"text": "Meeting link"}}}})
	c.relayEphemeral(&mattermost.Post{ChannelID: "ch1", Message: "  "})
	c.relayEphemeral(&mattermost.Post{Message: "no channel"})
	c.relayEphemeral(nil)
	if len(queued) != 1 {
		t.Fatalf("queued %d", len(queued))
	}
	m := queued[0].(*simplevent.Message[string])
	if m.PortalKey != (networkid.PortalKey{ID: "ch1", Receiver: c.UserLogin.ID}) || m.CreatePortal {
		t.Fatalf("portal %v create %v", m.PortalKey, m.CreatePortal)
	}
	conv, _ := m.ConvertMessageFunc(context.Background(), nil, nil, m.Data)
	content := conv.Parts[0].Content
	if content.MsgType != event.MsgNotice || !strings.Contains(content.FormattedBody, "<strong>away</strong>") || !strings.Contains(content.Body, "Meeting link") {
		t.Fatalf("content = %+v", content)
	}
	if conv.ThreadRoot == nil || *conv.ThreadRoot != "root1" {
		t.Fatalf("thread = %v", conv.ThreadRoot)
	}
}

func TestEphemeralNoticeExpiryWorkerEnabled(t *testing.T) {
	// Keep draining already-scheduled notices after a restart, even if the user
	// has disabled scheduling for new notices in the meantime.
	for _, enabled := range []bool{false, true} {
		connector := &MattermostConnector{Config: &Config{ExpireEphemeralNotices: enabled}}
		if !connector.GetCapabilities().DisappearingMessages {
			t.Errorf("expiry worker disabled with expire_ephemeral_notices=%t", enabled)
		}
	}
}

func TestEphemeralNoticeExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		wantTimer time.Duration
	}{
		{"off", false, 0},
		{"on", true, 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
			cfg := Config{ExpireEphemeralNotices: tc.enabled}
			c.UserLogin.Bridge = &bridgev2.Bridge{Network: &MattermostConnector{Config: &cfg}}
			var queued []*simplevent.Message[string]
			c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
				queued = append(queued, ev.(*simplevent.Message[string]))
				return bridgev2.EventHandlingResultQueued
			}
			c.relayEphemeral(&mattermost.Post{ChannelID: "ch1", Message: "Private reply"})
			c.sendNotice("ch1", "Bridge status")
			if len(queued) != 2 {
				t.Fatalf("queued %d notices, want 2", len(queued))
			}
			for i, m := range queued {
				conv, err := m.ConvertMessageFunc(context.Background(), nil, nil, m.Data)
				if err != nil {
					t.Fatal(err)
				}
				var want database.DisappearingSetting
				if i == 0 && tc.enabled {
					want = database.DisappearingSetting{Type: event.DisappearingTypeAfterSend, Timer: tc.wantTimer}
				}
				if conv.Disappear != want {
					t.Fatalf("notice %d: disappearance = %+v, want %+v", i, conv.Disappear, want)
				}
			}
		})
	}
}

func TestPlainNoticeStaysPlain(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return bridgev2.EventHandlingResultQueued
	}
	c.sendNotice("ch1", "**not bold**")
	m := queued[0].(*simplevent.Message[string])
	conv, _ := m.ConvertMessageFunc(context.Background(), nil, nil, m.Data)
	if conv.Parts[0].Content.FormattedBody != "" || conv.Parts[0].Content.Body != "**not bold**" || conv.ThreadRoot != nil {
		t.Fatalf("plain notice changed: %+v", conv.Parts[0].Content)
	}
}

func TestEphemeralEventRouted(t *testing.T) {
	p := newProcessEventsHarness(t, lifecycleOptions{})
	p.deliver(&mattermost.EphemeralEvent{Post: &mattermost.Post{ChannelID: "c1", Message: "hi"}})
	for _, ev := range p.events.drain() {
		if m, ok := ev.(*simplevent.Message[string]); ok && m.Data == "hi" {
			return
		}
	}
	t.Fatal("ephemeral message not relayed")
}

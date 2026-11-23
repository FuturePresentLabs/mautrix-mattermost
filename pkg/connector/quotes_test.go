package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// quoteServer is a Mattermost server for quoted replies: it serves posts and
// users, says whether the plugin is active, and records writes.
type quoteServer struct {
	mu          sync.Mutex
	posts       map[string]*mattermost.Post
	plugin      bool
	pluginCalls int
	postReads   []string
	created     []map[string]any
	patched     []map[string]any
	put         []map[string]any
}

func newQuoteServer(t *testing.T) (*quoteServer, *httptest.Server) {
	s := &quoteServer{plugin: true, posts: map[string]*mattermost.Post{
		"target": {ID: "target", UserID: "u1", ChannelID: "ch1", Message: "Shall we meet tomorrow?"},
		"gone":   {ID: "gone", UserID: "u1", DeleteAt: 5, Message: "deleted"},
		"mine":   {ID: "mine", Type: mattermost.QuotedReplyPostType, Message: "> **Erin Example**\n> Shall we meet tomorrow?\n\nyes", Props: map[string]any{mattermost.QuotedReplyToProp: "target", mattermost.QuotedReplyBodyProp: "yes"}},
		"plain":  {ID: "plain", Message: "plain post"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		switch {
		case r.URL.Path == "/api/v4/plugins/webapp":
			s.pluginCalls++
			if s.plugin {
				_, _ = w.Write([]byte(`[{"id":"` + mattermost.ChannelReplyPluginID + `"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case r.URL.Path == "/api/v4/users/u1":
			_ = json.NewEncoder(w).Encode(mattermost.User{ID: "u1", Username: "erin", FirstName: "Erin", LastName: "Example"})
		case r.Method == "POST" && r.URL.Path == "/api/v4/files":
			_, _ = w.Write([]byte(`{"file_infos":[{"id":"uploaded"}]}`))
		case r.Method == "POST" && r.URL.Path == "/api/v4/posts":
			s.created = append(s.created, body)
			root, _ := body["root_id"].(string)
			if body["message"] == "server-adjusted root" {
				root = "confirmed-root"
			}
			_ = json.NewEncoder(w).Encode(mattermost.Post{ID: "newpost", RootID: root})
		case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/patch"):
			s.patched = append(s.patched, body)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "PUT":
			s.put = append(s.put, body)
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/api/v4/posts/"):
			postID := strings.TrimPrefix(r.URL.Path, "/api/v4/posts/")
			s.postReads = append(s.postReads, postID)
			if p, ok := s.posts[postID]; ok {
				_ = json.NewEncoder(w).Encode(p)
				return
			}
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"status_code":404}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func quoteClient(srv *httptest.Server) *MattermostClient {
	c := &MattermostClient{UserID: "me", Token: "tok", baseURL: srv.URL}
	c.client = &mattermost.Client{}
	return c
}

func replyMsg(text string, replyTo, threadRoot *database.Message) *bridgev2.MatrixMessage {
	m := makeMatrixMessage("ch1", event.MsgText, text)
	m.ReplyTo, m.ThreadRoot = replyTo, threadRoot
	return m
}

func TestBeeperReplyBecomesQuotedReply(t *testing.T) {
	s, srv := newQuoteServer(t)
	c := quoteClient(srv)
	c.quotedRepliesFn = func() bool { return true }
	// A reply outside a thread, even to a message in a thread, goes to the channel.
	target := &database.Message{ID: "target", ThreadRoot: "someroot"}
	if _, err := c.HandleMatrixMessage(context.Background(), replyMsg("yes, 10am", target, nil)); err != nil {
		t.Fatal(err)
	}
	b := s.created[0]
	props, _ := b["props"].(map[string]any)
	if b["message"] != "> **Erin Example**\n> Shall we meet tomorrow?\n\nyes, 10am" || b["type"] != mattermost.QuotedReplyPostType ||
		props[mattermost.QuotedReplyToProp] != "target" || props[mattermost.QuotedReplyBodyProp] != "yes, 10am" {
		t.Fatalf("posted %v", b)
	}
	if _, ok := b["root_id"]; ok {
		t.Fatalf("reply outside a thread went to a thread: %v", b)
	}
	// Inside a thread, the quoted reply stays in the thread.
	root := &database.Message{ID: "root1"}
	if _, err := c.HandleMatrixMessage(context.Background(), replyMsg("in thread", target, root)); err != nil {
		t.Fatal(err)
	}
	if b := s.created[1]; b["root_id"] != "root1" || b["type"] != mattermost.QuotedReplyPostType {
		t.Fatalf("thread reply %v", b)
	}
}

func TestRepliesFallBackToThreads(t *testing.T) {
	s, srv := newQuoteServer(t)
	c := quoteClient(srv)
	target := &database.Message{ID: "target"}
	enabled := false
	c.quotedRepliesFn = func() bool { return enabled }
	send := func(text string, replyTo *database.Message) map[string]any {
		t.Helper()
		if _, err := c.HandleMatrixMessage(context.Background(), replyMsg(text, replyTo, nil)); err != nil {
			t.Fatal(err)
		}
		return s.created[len(s.created)-1]
	}
	if b := send("off", target); b["root_id"] != "target" || b["type"] != nil {
		t.Fatalf("disabled: %v", b)
	}
	enabled = true
	if b := send("plain message", nil); b["type"] != nil || b["props"] != nil {
		t.Fatalf("not a reply: %v", b)
	}
	if b := send("to a deleted post", &database.Message{ID: "gone"}); b["type"] != nil || b["root_id"] != "gone" {
		t.Fatalf("deleted target: %v", b)
	}
	if b := send("to a missing post", &database.Message{ID: "missing"}); b["type"] != nil || b["root_id"] != "missing" {
		t.Fatalf("missing target: %v", b)
	}
	if b := send("to a command", &database.Message{ID: networkid.MessageID(commandMessagePrefix + "x")}); b["type"] != nil {
		t.Fatalf("command target: %v", b)
	}
}

func TestPluginDetection(t *testing.T) {
	s, srv := newQuoteServer(t)
	c := quoteClient(srv)
	if !c.channelReplyPluginActive() || !c.channelReplyPluginActive() || s.pluginCalls != 1 {
		t.Fatalf("calls %d", s.pluginCalls)
	}
	s.plugin = false
	c.quotePluginChecked = time.Now().Add(-pluginCheckTTL - time.Second)
	if c.channelReplyPluginActive() || s.pluginCalls != 2 {
		t.Fatalf("not rechecked after the TTL: calls %d", s.pluginCalls)
	}
	down := &MattermostClient{Token: "tok", baseURL: "http://127.0.0.1:1"}
	if down.channelReplyPluginActive() {
		t.Fatal("unreachable server counted as having the plugin")
	}
	// The option gates the server check.
	cfgOff := &Config{QuotedReplies: false}
	off := &MattermostClient{Token: "tok", baseURL: srv.URL, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{}, Bridge: &bridgev2.Bridge{Network: &MattermostConnector{Config: cfgOff}}}}
	s.plugin = true
	if off.quotedRepliesEnabled() {
		t.Fatal("enabled with the option off")
	}
	cfgOn := &Config{QuotedReplies: true}
	on := &MattermostClient{Token: "tok", baseURL: srv.URL, UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{}, Bridge: &bridgev2.Bridge{Network: &MattermostConnector{Config: cfgOn}}}}
	if !on.quotedRepliesEnabled() {
		t.Fatal("disabled with the option and plugin on")
	}
	s.plugin = false
	noPlugin := &MattermostClient{Token: "tok", baseURL: srv.URL, UserLogin: on.UserLogin}
	if noPlugin.quotedRepliesEnabled() {
		t.Fatal("enabled without the plugin")
	}
	if (&MattermostClient{}).quotedRepliesEnabled() {
		t.Fatal("enabled without a config")
	}
}

func TestQuotedReplyArrivesAsReply(t *testing.T) {
	post := &mattermost.Post{ID: "p", ChannelID: "ch1", Type: mattermost.QuotedReplyPostType,
		Message: "> **Erin**\n> the question\n\nthe answer", Props: map[string]any{mattermost.QuotedReplyToProp: "target"}}
	replyTo, root := threadRelation(post)
	if replyTo == nil || replyTo.MessageID != "target" || root != nil {
		t.Fatalf("reply %v root %v", replyTo, root)
	}
	if got := postMarkdown(post); got != "the answer" {
		t.Fatalf("text %q", got)
	}
	post.RootID = "r"
	if replyTo, root = threadRelation(post); replyTo == nil || root == nil || *root != "r" {
		t.Fatal("quoted reply in a thread")
	}
	plain := &mattermost.Post{ID: "q", RootID: "r", Message: "> quote\n\nbody"}
	if replyTo, root = threadRelation(plain); replyTo != nil || root == nil {
		t.Fatal("thread reply got a reply target")
	}
	if postMarkdown(plain) != plain.Message {
		t.Fatal("plain post lost its quote")
	}
	c := &MattermostClient{}
	cm, err := c.convertPost(context.Background(), nil, "", &mattermost.Post{ID: "p", Type: mattermost.QuotedReplyPostType,
		Message: "> **Erin**\n> q\n\nok", Props: map[string]any{mattermost.QuotedReplyToProp: "target"}})
	if err != nil || cm.ReplyTo == nil || cm.ReplyTo.MessageID != "target" || cm.Parts[0].Content.Body != "ok" {
		t.Fatalf("converted %+v %v", cm, err)
	}
}

func TestEditKeepsQuote(t *testing.T) {
	s, srv := newQuoteServer(t)
	c := quoteClient(srv)
	edit := func(id, text string) {
		t.Helper()
		err := c.HandleMatrixEdit(context.Background(), &bridgev2.MatrixEdit{
			MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{Content: &event.MessageEventContent{MsgType: event.MsgText, Body: text}, Portal: makePortal("ch1")},
			EditTarget:      &database.Message{ID: networkid.MessageID(id)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	edit("mine", "yes, at 10")
	if len(s.patched) != 1 || s.patched[0]["message"] != "> **Erin Example**\n> Shall we meet tomorrow?\n\nyes, at 10" ||
		s.patched[0]["props"].(map[string]any)[mattermost.QuotedReplyBodyProp] != "yes, at 10" || s.patched[0]["props"].(map[string]any)[mattermost.QuotedReplyToProp] != "target" {
		t.Fatalf("patched %v", s.patched)
	}
	edit("plain", "changed")
	if len(s.put) != 1 || s.put[0]["message"] != "changed" || len(s.patched) != 1 {
		t.Fatalf("plain edit: put %v patched %v", s.put, s.patched)
	}
	edit("unknown", "changed again")
	if len(s.put) != 2 {
		t.Fatal("unreadable post not edited as before")
	}
}

func TestQuoteAuthor(t *testing.T) {
	if quoteAuthor(&mattermost.User{Username: "erin", FirstName: "Erin"}) != "Erin" || quoteAuthor(&mattermost.User{Username: "erin"}) != "erin" {
		t.Fatal("author name")
	}
}

type outboundMediaBot struct {
	bridgev2.MatrixAPI
}

func (outboundMediaBot) DownloadMedia(context.Context, id.ContentURIString, *event.EncryptedFileInfo) ([]byte, error) {
	return []byte("attachment contents"), nil
}

func TestHandleMatrixReplyThreadRoot(t *testing.T) {
	for _, tc := range []struct {
		name                string
		reply, thread       *database.Message
		plugin, media, read bool
		requestRoot, dbRoot string
	}{
		{name: "legacy reply", reply: &database.Message{ID: "target"}, read: true, requestRoot: "outer", dbRoot: "outer"},
		{name: "stored root", reply: &database.Message{ID: "target", ThreadRoot: "stored"}, requestRoot: "stored", dbRoot: "stored"},
		{name: "explicit thread", reply: &database.Message{ID: "target"}, thread: &database.Message{ID: "explicit"}, requestRoot: "explicit", dbRoot: "explicit"},
		{name: "lookup failure", reply: &database.Message{ID: "missing"}, read: true, requestRoot: "missing", dbRoot: "missing"},
		{name: "command", reply: &database.Message{ID: commandMessagePrefix + "example"}, plugin: true},
		{name: "command with stored root", reply: &database.Message{ID: commandMessagePrefix + "example", ThreadRoot: "stored"}, plugin: true, requestRoot: "stored", dbRoot: "stored"},
		{name: "native media reply", reply: &database.Message{ID: "target"}, media: true, read: true, requestRoot: "outer", dbRoot: "outer"},
		{name: "quoted plain reply", reply: &database.Message{ID: "target"}, plugin: true, read: true},
		{name: "quoted media reply", reply: &database.Message{ID: "target", ThreadRoot: "stored"}, plugin: true, media: true, read: true},
		{name: "quoted in thread", reply: &database.Message{ID: "target"}, thread: &database.Message{ID: "explicit"}, plugin: true, read: true, requestRoot: "explicit", dbRoot: "explicit"},
		{name: "quote lookup failure", reply: &database.Message{ID: "missing"}, plugin: true, read: true, requestRoot: "missing", dbRoot: "missing"},
		{name: "server-adjusted root", reply: &database.Message{ID: "target", ThreadRoot: "stored"}, requestRoot: "stored", dbRoot: "confirmed-root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, srv := newQuoteServer(t)
			s.posts["target"].RootID = "outer"
			c := quoteClient(srv)
			c.quotedRepliesFn = func() bool { return tc.plugin }
			msg := replyMsg(tc.name, tc.reply, tc.thread)
			if tc.media {
				c.UserLogin = &bridgev2.UserLogin{Bridge: &bridgev2.Bridge{Bot: outboundMediaBot{}}}
				msg.Content.MsgType = event.MsgFile
				msg.Content.FileName = "example.txt"
				msg.Content.URL = "mxc://example.test/file"
			}
			resp, err := c.HandleMatrixMessage(context.Background(), msg)
			if err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.created) != 1 {
				t.Fatalf("created posts = %v", s.created)
			}
			post := s.created[0]
			if root, _ := post["root_id"].(string); root != tc.requestRoot {
				t.Errorf("sent root = %q, want %q", root, tc.requestRoot)
			}
			if resp.DB.ID != "newpost" || resp.DB.SenderID != "me" || string(resp.DB.ThreadRoot) != tc.dbRoot {
				t.Errorf("DB message = %+v, want newpost from me with root %q", resp.DB, tc.dbRoot)
			}
			if tc.read {
				if len(s.postReads) != 1 || s.postReads[0] != string(tc.reply.ID) {
					t.Errorf("post reads = %v, want one read of %q", s.postReads, tc.reply.ID)
				}
			} else if len(s.postReads) != 0 {
				t.Errorf("unexpected post reads: %v", s.postReads)
			}
			if tc.plugin && tc.reply.ID == "target" && post["type"] != mattermost.QuotedReplyPostType {
				t.Errorf("post type = %v, want quoted reply", post["type"])
			}
			if tc.media {
				if tc.plugin && post["message"] != "> **Erin Example**\n> Shall we meet tomorrow?\n\n"+tc.name {
					t.Errorf("quoted media caption = %q", post["message"])
				}
				if files, ok := post["file_ids"].([]any); !ok || len(files) != 1 || files[0] != "uploaded" {
					t.Errorf("file_ids = %v, want uploaded attachment", post["file_ids"])
				}
			}
		})
	}
}

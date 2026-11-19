package mattermost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuoteBlock(t *testing.T) {
	if got := QuoteBlock("Erin Example", "line one\n\n  line   two "); got != "> **Erin Example**\n> line one line two" {
		t.Fatalf("got %q", got)
	}
	if got := QuoteBlock(" ", ""); got != "> **Unknown user**\n> Attachment" {
		t.Fatalf("empty: %q", got)
	}
	long := QuoteBlock("a", strings.Repeat("é", 600))
	body := strings.TrimPrefix(long, "> **a**\n> ")
	if r := []rune(body); len(r) != 500 || r[499] != '…' {
		t.Fatalf("truncated to %d runes, last %q", len(r), string(r[len(r)-1]))
	}
	if exact := QuoteBlock("a", strings.Repeat("x", 500)); strings.Contains(exact, "…") {
		t.Fatal("500 characters truncated")
	}
}

func TestQuotedReplyParsing(t *testing.T) {
	plain := &Post{ID: "p1", Message: "> just a quote\n\nhi"}
	if plain.IsQuotedReply() || plain.QuotedReplyTo() != "" || plain.QuotedReplyBody() != plain.Message {
		t.Fatal("a plain post with a quote is a quoted reply")
	}
	q := &Post{ID: "p2", Type: QuotedReplyPostType, Message: "> **Erin**\n> the question\n\nthe answer\n\nmore",
		Props: map[string]any{QuotedReplyToProp: "p1", QuotedReplyBodyProp: "stale"}}
	if !q.IsQuotedReply() || q.QuotedReplyTo() != "p1" || q.QuotedReplyBody() != "the answer\n\nmore" {
		t.Fatalf("to %q body %q", q.QuotedReplyTo(), q.QuotedReplyBody())
	}
	propsOnly := &Post{ID: "p3", Message: "edited away the quote", Props: map[string]any{QuotedReplyToProp: "p1", QuotedReplyBodyProp: "old"}}
	if !propsOnly.IsQuotedReply() || propsOnly.QuotedReplyBody() != "old" {
		t.Fatalf("props fallback: %q", propsOnly.QuotedReplyBody())
	}
	noBody := &Post{ID: "p4", Type: QuotedReplyPostType, Message: "no quote"}
	if noBody.QuotedReplyBody() != "no quote" {
		t.Fatal("message lost")
	}
	brokenQuote := &Post{ID: "p5", Type: QuotedReplyPostType, Message: "> **a**\nnot quoted\n\nbody"}
	if brokenQuote.QuotedReplyBody() != brokenQuote.Message {
		t.Fatalf("stripped a block with an unquoted line: %q", brokenQuote.QuotedReplyBody())
	}
	unterminated := &Post{ID: "p6", Type: QuotedReplyPostType, Message: "> **a**\n> only a quote"}
	if unterminated.QuotedReplyBody() != unterminated.Message {
		t.Fatal("stripped a quote with no body")
	}
	typeOnly := &Post{ID: "p9", Type: QuotedReplyPostType, Message: "> **a**\n> q\n\nbody"}
	if !typeOnly.IsQuotedReply() || typeOnly.QuotedReplyBody() != "body" {
		t.Fatalf("type without props: %q", typeOnly.QuotedReplyBody())
	}
	self := &Post{ID: "p7", Props: map[string]any{QuotedReplyToProp: "p7"}}
	if self.QuotedReplyTo() != "" || self.IsQuotedReply() {
		t.Fatal("a post quoting itself")
	}
	wrongType := &Post{ID: "p8", Props: map[string]any{QuotedReplyToProp: 42}}
	if wrongType.QuotedReplyTo() != "" {
		t.Fatal("non-string prop")
	}
	var nilPost *Post
	if nilPost.QuotedReplyTo() != "" || nilPost.IsQuotedReply() || nilPost.QuotedReplyBody() != "" {
		t.Fatal("nil post")
	}
}

func TestQuotedReplyBuild(t *testing.T) {
	target := &Post{ID: "t1", Type: QuotedReplyPostType, Message: "> **X**\n> older\n\nthe point", Props: map[string]any{QuotedReplyToProp: "t0"}}
	msg, typ, props := QuotedReply(target, "Erin", "agreed")
	if msg != "> **Erin**\n> the point\n\nagreed" || typ != QuotedReplyPostType || props[QuotedReplyToProp] != "t1" || props[QuotedReplyBodyProp] != "agreed" {
		t.Fatalf("%q %q %v", msg, typ, props)
	}
	if msg, _, _ := QuotedReply(&Post{ID: "t2", Message: "q"}, "Erin", ""); msg != "> **Erin**\n> q" {
		t.Fatalf("empty body: %q", msg)
	}
}

func TestQuotedReplyEdit(t *testing.T) {
	post := &Post{ID: "p", Type: QuotedReplyPostType, Message: "> **Erin**\n> q\n\nold text",
		Props: map[string]any{QuotedReplyToProp: "t", QuotedReplyBodyProp: "old text", "other": "kept"}}
	msg, props, ok := QuotedReplyEdit(post, "new text")
	if !ok || msg != "> **Erin**\n> q\n\nnew text" || props[QuotedReplyBodyProp] != "new text" || props["other"] != "kept" || props[QuotedReplyToProp] != "t" {
		t.Fatalf("%v %q %v", ok, msg, props)
	}
	if post.Props[QuotedReplyBodyProp] != "old text" {
		t.Fatal("edit changed the original props")
	}
	noQuote := &Post{ID: "p", Message: "plain now", Props: map[string]any{QuotedReplyToProp: "t"}}
	if msg, _, ok := QuotedReplyEdit(noQuote, "x"); !ok || msg != "x" {
		t.Fatalf("no quote: %q", msg)
	}
	if _, _, ok := QuotedReplyEdit(&Post{ID: "p", Message: "plain"}, "x"); ok {
		t.Fatal("plain post edited as a quoted reply")
	}
}

func TestSendPostWithAndPatch(t *testing.T) {
	var got []map[string]any
	var methods, paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body)
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"new"}`))
	}))
	defer srv.Close()
	id, err := SendPostWith(srv.URL, "tok", NewPost{ChannelID: "c", Message: "m", RootID: "r", FileIDs: []string{"f"}, Type: QuotedReplyPostType, Props: map[string]any{QuotedReplyToProp: "t"}})
	if err != nil || id != "new" {
		t.Fatalf("%q %v", id, err)
	}
	b := got[0]
	if b["channel_id"] != "c" || b["message"] != "m" || b["root_id"] != "r" || b["type"] != QuotedReplyPostType || b["props"].(map[string]any)[QuotedReplyToProp] != "t" || len(b["file_ids"].([]any)) != 1 {
		t.Fatalf("body %v", b)
	}
	if _, err := SendPost(srv.URL, "tok", "c", "plain", ""); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"type", "props", "root_id", "file_ids"} {
		if _, ok := got[1][k]; ok {
			t.Errorf("plain post sent %s", k)
		}
	}
	if err := PatchPost(srv.URL, "tok", "p1", "new", map[string]any{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if methods[2] != "PUT" || paths[2] != "/api/v4/posts/p1/patch" || got[2]["message"] != "new" || got[2]["props"].(map[string]any)["a"] != "b" {
		t.Fatalf("patch %s %s %v", methods[2], paths[2], got[2])
	}
	if err := PatchPost(srv.URL, "tok", "p1", "new", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := got[3]["props"]; ok {
		t.Fatal("nil props sent")
	}
}

func TestWebappPluginActive(t *testing.T) {
	list := `[{"id":"other"},{"id":"` + ChannelReplyPluginID + `"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/plugins/webapp" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(list))
	}))
	defer srv.Close()
	if ok, err := WebappPluginActive(srv.URL, "tok", ChannelReplyPluginID); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if ok, _ := WebappPluginActive(srv.URL, "tok", "missing"); ok {
		t.Fatal("missing plugin active")
	}
	list = `oops`
	if _, err := WebappPluginActive(srv.URL, "tok", ChannelReplyPluginID); err == nil {
		t.Fatal("bad answer accepted")
	}
}

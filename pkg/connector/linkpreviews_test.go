package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")

type recordingUpload struct {
	bridgev2.MatrixAPI
	uploads []string
}

func (r *recordingUpload) UploadMedia(_ context.Context, _ id.RoomID, data []byte, name, mime string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	r.uploads = append(r.uploads, mime)
	return "mxc://example.com/img", &event.EncryptedFileInfo{URL: "mxc://example.com/img"}, nil
}

const ogData = `{"type":"article","url":"https://news.example.com/a","title":"A headline","description":"What happened","site_name":"Example News",
"images":[{"url":"http://img.example.com/a.jpg","secure_url":"https://img.example.com/a.jpg"}]}`

func previewPost(data string) *mattermost.Post {
	return &mattermost.Post{ID: "p1", UserID: "alice", Message: "https://news.example.com/a?ref=1",
		Metadata: &mattermost.PostMetadata{
			Embeds: []*mattermost.PostEmbed{{Type: "opengraph", URL: "https://news.example.com/a?ref=1", Data: []byte(data)}},
			Images: map[string]*mattermost.PostImage{"https://img.example.com/a.jpg": {Width: 1024, Height: 576}},
		}}
}

func previewClient(fetch func(string) ([]byte, string, error)) *MattermostClient {
	c := newBackfillClient("http://127.0.0.1:1")
	c.fetchPreviewImageFn = fetch
	return c
}

func TestLinkPreviewBundled(t *testing.T) {
	var fetched []string
	c := previewClient(func(src string) ([]byte, string, error) {
		fetched = append(fetched, src)
		return checkPreviewImage(pngBytes, "")
	})
	up := &recordingUpload{}
	res, err := c.convertPost(context.Background(), up, "!r:example.com", previewPost(ogData))
	if err != nil {
		t.Fatal(err)
	}
	lps := res.Parts[0].Content.BeeperLinkPreviews
	if len(lps) != 1 {
		t.Fatalf("previews = %d", len(lps))
	}
	lp := lps[0]
	if lp.MatchedURL != "https://news.example.com/a?ref=1" || lp.CanonicalURL != "https://news.example.com/a" ||
		lp.Title != "A headline" || lp.Description != "What happened" || lp.SiteName != "Example News" || lp.Type != "article" {
		t.Fatalf("preview = %+v", lp.LinkPreview)
	}
	if strings.Join(fetched, ",") != "https://img.example.com/a.jpg" {
		t.Fatalf("fetched %v, want the secure URL once", fetched)
	}
	if lp.ImageURL != "mxc://example.com/img" || lp.ImageEncryption == nil || lp.ImageType != "image/png" ||
		lp.ImageWidth != 1024 || lp.ImageHeight != 576 || int(lp.ImageSize) != len(pngBytes) {
		t.Fatalf("image = %+v enc %v", lp.LinkPreview, lp.ImageEncryption)
	}
	if strings.Join(up.uploads, ",") != "image/png" {
		t.Fatalf("uploads %v", up.uploads)
	}
}

func TestLinkPreviewWithoutImage(t *testing.T) {
	c := previewClient(func(string) ([]byte, string, error) { return nil, "", errors.New("down") })
	res, _ := c.convertPost(context.Background(), &recordingUpload{}, "!r:example.com", previewPost(ogData))
	lp := res.Parts[0].Content.BeeperLinkPreviews[0]
	if lp.Title != "A headline" || lp.ImageURL != "" {
		t.Fatalf("preview = %+v", lp.LinkPreview)
	}
	plain := previewPost(`{"title":"T","images":[{"url":"http://img.example.com/b.png","width":10,"height":20}]}`)
	c.fetchPreviewImageFn = func(src string) ([]byte, string, error) { return checkPreviewImage(pngBytes, "") }
	res, _ = c.convertPost(context.Background(), &recordingUpload{}, "!r:example.com", plain)
	lp = res.Parts[0].Content.BeeperLinkPreviews[0]
	if lp.ImageWidth != 10 || lp.ImageHeight != 20 {
		t.Fatalf("og size not used: %+v", lp.LinkPreview)
	}
	// The page's own size wins over the server's measurement when both exist.
	both := previewPost(`{"title":"T","images":[{"url":"https://img.example.com/a.jpg","width":30,"height":40}]}`)
	res, _ = c.convertPost(context.Background(), &recordingUpload{}, "!r:example.com", both)
	if lp = res.Parts[0].Content.BeeperLinkPreviews[0]; lp.ImageWidth != 30 || lp.ImageHeight != 40 {
		t.Fatalf("og size overridden: %+v", lp.LinkPreview)
	}
}

func TestLinkPreviewsSkipped(t *testing.T) {
	c := previewClient(func(string) ([]byte, string, error) { t.Fatal("fetched"); return nil, "", nil })
	cases := map[string]*mattermost.Post{
		"no metadata": {ID: "p", Message: "hi"},
		"not og":      {ID: "p", Message: "x", Metadata: &mattermost.PostMetadata{Embeds: []*mattermost.PostEmbed{{Type: "image", Data: []byte(ogData)}}}},
		"empty og":    {ID: "p", Message: "x", Metadata: &mattermost.PostMetadata{Embeds: []*mattermost.PostEmbed{{Type: "opengraph", Data: []byte(`{"url":"https://a"}`)}}}},
		"bad json":    {ID: "p", Message: "x", Metadata: &mattermost.PostMetadata{Embeds: []*mattermost.PostEmbed{{Type: "opengraph", Data: []byte(`{`)}}}},
		"no data":     {ID: "p", Message: "x", Metadata: &mattermost.PostMetadata{Embeds: []*mattermost.PostEmbed{{Type: "opengraph"}}}},
		"nil embed":   {ID: "p", Message: "x", Metadata: &mattermost.PostMetadata{Embeds: []*mattermost.PostEmbed{nil}}},
	}
	for name, post := range cases {
		res, err := c.convertPost(context.Background(), &recordingUpload{}, "!r", post)
		if err != nil || len(res.Parts[0].Content.BeeperLinkPreviews) != 0 {
			t.Errorf("%s: previews %v err %v", name, res.Parts[0].Content.BeeperLinkPreviews, err)
		}
	}
	// No intent (no upload possible): text preview only, no fetch.
	res, _ := c.convertPost(context.Background(), nil, "!r", previewPost(ogData))
	if lp := res.Parts[0].Content.BeeperLinkPreviews; len(lp) != 1 || lp[0].ImageURL != "" {
		t.Fatalf("no-intent preview = %+v", lp)
	}
}

func TestLinkPreviewLimitAndFallbackURL(t *testing.T) {
	c := previewClient(func(string) ([]byte, string, error) { return nil, "", errors.New("x") })
	post := &mattermost.Post{ID: "p", Message: "links", Metadata: &mattermost.PostMetadata{}}
	for i := 0; i < 5; i++ {
		post.Metadata.Embeds = append(post.Metadata.Embeds, &mattermost.PostEmbed{Type: "opengraph", Data: []byte(`{"url":"https://a.example.com/","title":"T"}`)})
	}
	res, _ := c.convertPost(context.Background(), &recordingUpload{}, "!r", post)
	lps := res.Parts[0].Content.BeeperLinkPreviews
	if len(lps) != maxPreviews || lps[0].MatchedURL != "https://a.example.com/" {
		t.Fatalf("previews %d matched %q", len(lps), lps[0].MatchedURL)
	}
}

func TestPreviewImageChecks(t *testing.T) {
	var hooked []string
	guarded := &MattermostClient{fetchPreviewImageFn: func(src string) ([]byte, string, error) {
		hooked = append(hooked, src)
		return pngBytes, "image/png", nil
	}}
	for _, src := range []string{"file:///etc/passwd", "ftp://x/y.png", "https:///nohost", "http://", "::bad"} {
		if _, _, err := guarded.fetchPreviewImage(context.Background(), src); err == nil {
			t.Errorf("%q fetched", src)
		}
	}
	if len(hooked) != 0 {
		t.Fatalf("unsafe URLs reached the fetch: %v", hooked)
	}
	c := &MattermostClient{}
	if _, _, err := checkPreviewImage([]byte("<html>"), "text/html"); err == nil {
		t.Error("HTML accepted as an image")
	}
	if _, mime, err := checkPreviewImage([]byte("rawdata"), "image/webp; q=1"); err != nil || mime != "image/webp" {
		t.Errorf("declared type: %q %v", mime, err)
	}
	if _, _, err := checkPreviewImage(make([]byte, maxPreviewImageBytes+1), "image/png"); err == nil {
		t.Error("oversized image accepted")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(pngBytes) // an image body must not hide the error
			return
		}
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()
	if data, mime, err := c.fetchPreviewImage(context.Background(), srv.URL+"/a.png"); err != nil || mime != "image/png" || len(data) != len(pngBytes) {
		t.Fatalf("fetch: %q %v", mime, err)
	}
	if _, _, err := c.fetchPreviewImage(context.Background(), srv.URL+"/missing"); err == nil {
		t.Fatal("404 accepted")
	}
}

func TestCaptionedFileKeepsPreview(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("file")) }))
	defer s.Close()
	c := newBackfillClient(s.URL)
	c.fetchPreviewImageFn = func(string) ([]byte, string, error) { return nil, "", errors.New("x") }
	post := previewPost(ogData)
	post.FileIDs = []string{"f1"}
	res, _ := c.convertPost(context.Background(), &recordingUpload{}, "!r", post)
	if len(res.Parts[0].Content.BeeperLinkPreviews) != 1 {
		t.Fatal("caption lost its preview")
	}
}

func TestSystemPostsAreBotNotices(t *testing.T) {
	c := newBackfillClient("http://127.0.0.1:1")
	c.UserID = "me"
	post := &mattermost.Post{ID: "s1", UserID: "me", Type: "system_add_to_channel", Message: "bob added to the channel by me.", RootID: "root"}
	res, err := c.convertPost(context.Background(), nil, "!r", post)
	if err != nil || len(res.Parts) != 1 || res.Parts[0].Content.MsgType != event.MsgNotice || res.Parts[0].Content.Body != post.Message {
		t.Fatalf("converted %+v err %v", res, err)
	}
	if res.ThreadRoot == nil || *res.ThreadRoot != "root" {
		t.Fatalf("thread = %v", res.ThreadRoot)
	}
	if s := c.postSender(post); s.Sender != "" || s.IsFromMe || s.SenderLogin != "" {
		t.Fatalf("system sender = %+v, want the bridge bot", s)
	}
	own := &mattermost.Post{ID: "m", UserID: "me", Message: "hi"}
	if s := c.postSender(own); !s.IsFromMe || s.Sender != "me" {
		t.Fatalf("own sender = %+v", s)
	}
	other := &mattermost.Post{ID: "o", UserID: "bob", Type: "custom_plugin", Message: "hi"}
	if s := c.postSender(other); s.IsFromMe || s.Sender != "bob" {
		t.Fatalf("plugin post sender = %+v", s)
	}
	for _, typ := range []string{"system_channel_deleted", "system_join_channel", "system_header_change"} {
		if !(&mattermost.Post{Type: typ}).IsSystem() {
			t.Errorf("%s not a system post", typ)
		}
	}
	if (&mattermost.Post{Type: ""}).IsSystem() || (&mattermost.Post{Type: "custom_meet"}).IsSystem() {
		t.Error("user post treated as system")
	}
}

func TestLiveAndBackfillUseSystemSender(t *testing.T) {
	p := newProcessEventsHarness(t, lifecycleOptions{})
	p.client.UserID = "me"
	p.deliver(&mattermost.PostedEvent{ChannelID: "c1", Post: &mattermost.Post{ID: "s", ChannelID: "c1", UserID: "me", Type: "system_join_channel", Message: "me joined the channel."}})
	for _, ev := range p.events.drain() {
		if m, ok := ev.(*simplevent.Message[*mattermost.Post]); ok {
			if m.Sender.Sender != "" || m.Sender.IsFromMe {
				t.Fatalf("live system post sender = %+v", m.Sender)
			}
			goto backfill
		}
	}
	t.Fatal("system post not queued")
backfill:
	c := newBackfillClient("http://127.0.0.1:1")
	c.UserID = "me"
	bm := c.newBackfillMessage(context.Background(), nil, &bridgev2.Portal{Portal: &database.Portal{}}, &mattermost.Post{ID: "s", UserID: "me", Type: "system_join_channel", Message: "x"}, "")
	if bm.Sender.Sender != "" || bm.Sender.IsFromMe {
		t.Fatalf("backfill system sender = %+v", bm.Sender)
	}
}

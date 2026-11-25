package mattermost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPostBurnOnReadAccessors(t *testing.T) {
	expire := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	raw := `{"id":"p1","type":"burn_on_read","props":{"expire_at":` + itoa(expire.UnixMilli()) + `,"read_duration":600000},"metadata":{"expire_at":` + itoa(expire.Add(-time.Hour).UnixMilli()) + `}}`
	var p Post
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if !p.IsBurnOnRead() {
		t.Error("IsBurnOnRead() = false for a burn_on_read post")
	}
	if got := p.BurnExpiresAt(); !got.Equal(expire) {
		t.Errorf("BurnExpiresAt() = %v, want %v", got, expire)
	}
	if got := p.BurnReadDuration(); got != 10*time.Minute {
		t.Errorf("BurnReadDuration() = %v, want 10m", got)
	}
	if got := p.RevealExpiresAt(); !got.Equal(expire.Add(-time.Hour)) {
		t.Errorf("RevealExpiresAt() = %v, want %v", got, expire.Add(-time.Hour))
	}
}

func TestPostBurnOnReadAccessorsAbsent(t *testing.T) {
	var p Post
	if p.IsBurnOnRead() {
		t.Error("an empty post is not burn-on-read")
	}
	if !p.BurnExpiresAt().IsZero() || !p.RevealExpiresAt().IsZero() || p.BurnReadDuration() != 0 {
		t.Error("missing props and metadata must read as zero values")
	}
	var normal Post
	_ = json.Unmarshal([]byte(`{"type":"","props":{"expire_at":"not a number"}}`), &normal)
	if normal.IsBurnOnRead() || !normal.BurnExpiresAt().IsZero() {
		t.Error("a malformed expire_at must read as zero, not panic")
	}
}

func TestRevealPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v4/posts/p1/reveal" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("missing bearer token")
		}
		_, _ = w.Write([]byte(`{"id":"p1","type":"burn_on_read","message":"hello","metadata":{"expire_at":42}}`))
	}))
	defer srv.Close()
	post, err := RevealPost(srv.URL, "tok", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if post.Message != "hello" || post.Metadata == nil || post.Metadata.ExpireAt != 42 {
		t.Errorf("unexpected revealed post %+v", post)
	}
}

func TestRevealPostFailsLoudly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	if _, err := RevealPost(srv.URL, "tok", "p1"); err == nil {
		t.Fatal("a refused reveal must be an error")
	}
}

func TestBurnPost(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := BurnPost(srv.URL, "tok", "p9"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/api/v4/posts/p9/burn" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
}

func TestGetBurnOnReadSettings(t *testing.T) {
	cases := []struct {
		name string
		body string
		want BurnOnReadSettings
	}{
		{"enabled", `{"EnableBurnOnRead":"true","BurnOnReadDurationSeconds":"600"}`, BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute}},
		{"disabled", `{"EnableBurnOnRead":"false","BurnOnReadDurationSeconds":"600"}`, BurnOnReadSettings{}},
		{"absent (server without the feature)", `{}`, BurnOnReadSettings{}},
		{"enabled without a usable duration is not enabled", `{"EnableBurnOnRead":"true","BurnOnReadDurationSeconds":"abc"}`, BurnOnReadSettings{}},
		{"maximum lifetime follows the server", `{"EnableBurnOnRead":"true","BurnOnReadDurationSeconds":"600","BurnOnReadMaximumTimeToLiveSeconds":"259200"}`, BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute, MaxTTL: 72 * time.Hour}},
		{"maximum lifetime not reported stays unknown", `{"EnableBurnOnRead":"true","BurnOnReadDurationSeconds":"600"}`, BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute}},
		{"unusable maximum lifetime stays unknown", `{"EnableBurnOnRead":"true","BurnOnReadDurationSeconds":"600","BurnOnReadMaximumTimeToLiveSeconds":"abc"}`, BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute}},
		{"zero maximum lifetime stays unknown", `{"EnableBurnOnRead":"true","BurnOnReadDurationSeconds":"600","BurnOnReadMaximumTimeToLiveSeconds":"0"}`, BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute}},
		{"negative maximum lifetime stays unknown", `{"EnableBurnOnRead":"true","BurnOnReadDurationSeconds":"600","BurnOnReadMaximumTimeToLiveSeconds":"-5"}`, BurnOnReadSettings{Enabled: true, Duration: 10 * time.Minute}},
		{"disabled ignores the lifetime", `{"EnableBurnOnRead":"false","BurnOnReadDurationSeconds":"600","BurnOnReadMaximumTimeToLiveSeconds":"259200"}`, BurnOnReadSettings{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v4/config/client" || r.URL.Query().Get("format") != "old" {
					t.Errorf("unexpected request %s", r.URL.String())
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := GetBurnOnReadSettings(srv.URL, "tok")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("settings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// post_revealed carries the revealed post to the revealing user's own sessions,
// which includes the bridge. The event must keep identifiers and the expiry,
// never the text or the file list.
func TestParsePostRevealedKeepsNoContent(t *testing.T) {
	c := NewClient("http://example.invalid", "tok")
	post := `{"id":"p1","channel_id":"c1","user_id":"u2","type":"burn_on_read","message":"SECRET PAYLOAD","file_ids":["f1"],"metadata":{"expire_at":1234,"files":[{"id":"f1","name":"SECRET.png"}]}}`
	frame, _ := json.Marshal(map[string]any{"event": "post_revealed", "data": map[string]any{"post": post}})
	c.parseEvent(frame)
	evt := (<-c.Events).(*PostRevealedEvent)
	if evt.PostID != "p1" || evt.ChannelID != "c1" || evt.AuthorID != "u2" || evt.ExpireAt != 1234 {
		t.Errorf("unexpected event %+v", evt)
	}
	dump, _ := json.Marshal(evt)
	if strings.Contains(string(dump), "SECRET") {
		t.Errorf("revealed content leaked into the event: %s", dump)
	}
}

func TestParsePostBurned(t *testing.T) {
	c := NewClient("http://example.invalid", "tok")
	frame, _ := json.Marshal(map[string]any{
		"event":     "post_burned",
		"data":      map[string]any{"post_id": "p1"},
		"broadcast": map[string]any{"channel_id": "c1"},
	})
	c.parseEvent(frame)
	evt := (<-c.Events).(*PostBurnedEvent)
	if evt.PostID != "p1" || evt.ChannelID != "c1" {
		t.Errorf("unexpected event %+v", evt)
	}
}

func TestParseAllRevealed(t *testing.T) {
	c := NewClient("http://example.invalid", "tok")
	frame, _ := json.Marshal(map[string]any{
		"event":     "burn_on_read_all_revealed",
		"data":      map[string]any{"post_id": "p1", "sender_expire_at": 5555},
		"broadcast": map[string]any{"channel_id": "c1"},
	})
	c.parseEvent(frame)
	evt := (<-c.Events).(*PostAllRevealedEvent)
	if evt.PostID != "p1" || evt.ChannelID != "c1" || evt.SenderExpireAt != 5555 {
		t.Errorf("unexpected event %+v", evt)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestRevealedForViewer(t *testing.T) {
	for name, tc := range map[string]struct {
		post *Post
		want bool
	}{
		"unrevealed: empty message, no metadata":   {&Post{Type: PostTypeBurnOnRead}, false},
		"unrevealed: metadata wiped by the server": {&Post{Type: PostTypeBurnOnRead, Metadata: &PostMetadata{}}, false},
		"unrevealed: priority survives the wipe":   {&Post{Type: PostTypeBurnOnRead, Metadata: &PostMetadata{Priority: &PostPriority{}}}, false},
		"revealed: the text came back":             {&Post{Type: PostTypeBurnOnRead, Message: "text"}, true},
		"revealed: this user's expiry is set":      {&Post{Type: PostTypeBurnOnRead, Metadata: &PostMetadata{ExpireAt: 5}}, true},
		"nil post":                                 {nil, false},
	} {
		if got := tc.post.RevealedForViewer(); got != tc.want {
			t.Errorf("%s: RevealedForViewer() = %t, want %t", name, got, tc.want)
		}
	}
}

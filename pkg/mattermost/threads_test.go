package mattermost

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollapsedThreads(t *testing.T) {
	cases := []struct {
		setting, pref string
		prefStatus    int
		want          bool
	}{
		{"always_on", "off", 200, true},
		{"disabled", "on", 200, false},
		{"", "on", 200, false},
		{"default_on", "", 400, true},
		{"default_off", "", 400, false},
		{"default_off", "", 404, false},
		{"default_off", "on", 200, true},
		{"default_on", "off", 200, false},
		{"default_on", "weird", 200, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v4/config/client":
				_ = json.NewEncoder(w).Encode(map[string]string{"CollapsedThreads": tc.setting})
			case "/api/v4/users/me/preferences/display_settings/name/collapsed_reply_threads":
				if tc.prefStatus != 200 {
					w.WriteHeader(tc.prefStatus)
					fmt.Fprintf(w, `{"id":"app.preference.get.app_error","status_code":%d}`, tc.prefStatus)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"value": tc.pref})
			default:
				w.WriteHeader(404)
			}
		}))
		got, err := CollapsedThreads(srv.URL, "tok")
		srv.Close()
		if err != nil || got != tc.want {
			t.Errorf("%+v: got %v %v", tc, got, err)
		}
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/config/client" {
			_, _ = w.Write([]byte(`{"CollapsedThreads":"default_on"}`))
			return
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"id":"other","status_code":500}`))
	}))
	defer broken.Close()
	if _, err := CollapsedThreads(broken.URL, "tok"); err == nil {
		t.Fatal("server error swallowed")
	}
}

func TestGetUserThreadsPages(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v4/users/me/teams/tm/threads") {
			w.WriteHeader(404)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		n := 100
		if page == "1" {
			n = 30
		}
		var ths []map[string]any
		for i := 0; i < n; i++ {
			ths = append(ths, map[string]any{"id": fmt.Sprintf("p%s-%d", page, i), "last_viewed_at": 5, "unread_replies": 0, "post": map[string]any{"channel_id": "c"}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 130, "threads": ths})
	}))
	defer srv.Close()
	got, err := GetUserThreads(srv.URL, "tok", "tm")
	if err != nil || len(got) != 130 || strings.Join(pages, ",") != "0,1" || got[0].Post.ChannelID != "c" || got[0].LastViewedAt != 5 {
		t.Fatalf("%d %v %v", len(got), pages, err)
	}
}

func TestThreadReadEventDecode(t *testing.T) {
	e := threadReadEvent(WSEvent{Event: "thread_read_changed", Data: map[string]interface{}{"thread_id": "r", "channel_id": "c", "timestamp": float64(42)}})
	if e == nil || e.ThreadID != "r" || e.ChannelID != "c" || e.Timestamp != 42 {
		t.Fatalf("%+v", e)
	}
	e = threadReadEvent(WSEvent{Data: map[string]interface{}{"thread_id": "r"}, Broadcast: WSBroadcast{ChannelID: "b"}})
	if e == nil || e.ChannelID != "b" {
		t.Fatalf("broadcast channel: %+v", e)
	}
	if threadReadEvent(WSEvent{Data: map[string]interface{}{"channel_id": "c"}}) != nil {
		t.Fatal("no thread")
	}
	c := &Client{Events: make(chan any, 1)}
	c.parseEvent([]byte(`{"event":"thread_read_changed","data":{"thread_id":"r","channel_id":"c","timestamp":42}}`))
	select {
	case ev := <-c.Events:
		if e, ok := ev.(*ThreadReadEvent); !ok || e.ThreadID != "r" {
			t.Fatalf("%#v", ev)
		}
	default:
		t.Fatal("thread_read_changed not delivered")
	}
}

func TestGetUserThreadsStopsAtTotal(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var ths []map[string]any
		for i := 0; i < 100; i++ {
			ths = append(ths, map[string]any{"id": fmt.Sprintf("p%d-%d", calls, i)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 100, "threads": ths})
	}))
	defer srv.Close()
	got, err := GetUserThreads(srv.URL, "tok", "tm")
	if err != nil || len(got) != 100 || calls != 1 {
		t.Fatalf("%d threads, %d calls, %v", len(got), calls, err)
	}
}

func TestGetUnreadThreadIDs(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 2, "threads": []map[string]any{{"id": "a"}, {"id": ""}, {"id": "b"}}})
	}))
	defer srv.Close()
	ids, err := GetUnreadThreadIDs(srv.URL, "tok", "tm")
	if err != nil || strings.Join(ids, ",") != "a,b" || !strings.Contains(query, "unread=true") {
		t.Fatalf("%v %v %q", ids, err, query)
	}
	if _, err := GetUnreadThreadIDs("http://127.0.0.1:1", "tok", "tm"); err == nil {
		t.Fatal("error swallowed")
	}
	_, _ = GetUserThreads(srv.URL, "tok", "tm")
	if strings.Contains(query, "unread") {
		t.Fatal("followed threads filtered to unread")
	}
}

package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A lifecycle answer must come from the endpoint that was asked, so a redirect
// is an error rather than something to follow. Otherwise whatever the redirect
// points at could answer "this channel is gone".
func TestGetChannelContextRefusesRedirect(t *testing.T) {
	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/channels/chan1":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		default:
			followed = true
			_, _ = w.Write([]byte(`{"id":"chan1","delete_at":1}`))
		}
	}))
	defer srv.Close()

	_, err := GetChannelContext(context.Background(), srv.URL, "tok", "chan1")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusFound {
		t.Fatalf("err = %v, want the redirect reported as HTTP 302", err)
	}
	if followed {
		t.Fatal("the redirect target was requested")
	}
}

func TestGetChannelContextDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Error("missing Authorization header")
		}
		_, _ = w.Write([]byte(`{"id":"chan1","team_id":"team1","delete_at":7}`))
	}))
	defer srv.Close()

	ch, err := GetChannelContext(context.Background(), srv.URL, "tok", "chan1")
	if err != nil {
		t.Fatal(err)
	}
	if ch.ID != "chan1" || ch.TeamID != "team1" || ch.DeleteAt != 7 {
		t.Fatalf("channel = %+v", ch)
	}
}

func TestGetSelfContextRefusesRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/somewhere-else", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	if _, _, err := GetSelfContext(context.Background(), srv.URL, "tok"); err == nil {
		t.Fatal("a redirected identity check must fail")
	}
}

// The ordinary listing omits archived channels, so the lifecycle poll uses this
// one. Archived entries must arrive with their delete_at intact.
func TestGetAllChannelsIncludingArchivedIncludesArchived(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/users/user1/channels" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`[
			{"id":"active","delete_at":0},
			{"id":"archived","delete_at":1699999999000}
		]`))
	}))
	defer srv.Close()

	channels, err := GetAllChannelsIncludingArchived(context.Background(), srv.URL, "tok", "user1")
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "include_deleted=true" {
		t.Fatalf("query = %q, want include_deleted=true", gotQuery)
	}
	if len(channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(channels))
	}
	if ch := channels["active"]; ch == nil || ch.DeleteAt != 0 {
		t.Fatalf("active = %+v", channels["active"])
	}
	if ch := channels["archived"]; ch == nil || ch.DeleteAt != 1699999999000 {
		t.Fatalf("archived = %+v", channels["archived"])
	}
}

// An entry that cannot be read unambiguously must fail the whole listing: a
// partial or guessed answer would let a channel's state be decided by a payload
// the server did not actually give.
func TestGetAllChannelsIncludingArchivedRejectsUnusableListings(t *testing.T) {
	cases := map[string]string{
		"missing delete_at":  `[{"id":"a"}]`,
		"negative delete_at": `[{"id":"a","delete_at":-1}]`,
		"blank id":           `[{"id":"","delete_at":0}]`,
		"duplicate id":       `[{"id":"a","delete_at":0},{"id":"a","delete_at":0}]`,
		"not an array":       `{"id":"a","delete_at":0}`,
		"null":               `null`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer srv.Close()
			if _, err := GetAllChannelsIncludingArchived(context.Background(), srv.URL, "tok", "user1"); err == nil {
				t.Fatalf("payload %s was accepted", payload)
			}
		})
	}
}

func TestGetAllChannelsIncludingArchivedRequiresUserID(t *testing.T) {
	if _, err := GetAllChannelsIncludingArchived(context.Background(), "http://127.0.0.1:1", "tok", ""); err == nil {
		t.Fatal("listing without a user ID must fail before any request")
	}
}

// Ordinary GETs keep following redirects: only evidence-bearing requests refuse
// them.
func TestDoGetContextStillFollowsRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"id":"final"}`))
	}))
	defer srv.Close()

	var out struct {
		ID string `json:"id"`
	}
	if err := doGetContext(context.Background(), srv.URL, "tok", "/old", &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "final" {
		t.Fatalf("id = %q", out.ID)
	}
}

// A non-200 answer is reported with Mattermost's structured error, whichever
// status produced it.
func TestDoGetReportsStructuredErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "api.context.404.app_error", "message": "gone", "status_code": 404,
		})
	}))
	defer srv.Close()

	var out struct{}
	err := doGetContext(context.Background(), srv.URL, "tok", "/x", &out)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v", err)
	}
	if httpErr.StatusCode != 404 || httpErr.ErrorID != "api.context.404.app_error" || httpErr.Message != "gone" {
		t.Fatalf("error = %+v", httpErr)
	}
}

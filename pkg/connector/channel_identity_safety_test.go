package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A delete probe is only evidence if the token that answered it belongs to the
// logged-in user. Otherwise a revoked, swapped or proxied token could report a
// channel as gone and take the room with it.
func TestDeleteProbeMustMatchLoginIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/users/me":
			// Authenticated, but as somebody else.
			_, _ = w.Write([]byte(`{"id":"wrong-user","username":"wrong"}`))
		case "/api/v4/channels/channel":
			// An authoritative "no such channel", which would justify deleting
			// the room if the identity check did not run first.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"app.channel.get.existing.app_error","message":"not found","status_code":404}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	c.ServerURL = srv.URL
	c.UserID = "expected-user"

	_, outcome := c.probeChannelLifecycle(context.Background(), "channel", true)
	if outcome == lifecycleDeleted {
		t.Fatal("a channel was proven deleted using a token belonging to a different user")
	}
	if outcome != lifecycleInconclusive {
		t.Fatalf("outcome = %v, want inconclusive", outcome)
	}
}

// The same probe from the expected user is evidence, so deletions still work:
// the guard rejects the wrong identity, not deletions in general.
func TestDeleteProbeFromTheExpectedUserIsEvidence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/users/me":
			_, _ = w.Write([]byte(`{"id":"expected-user","username":"me"}`))
		case "/api/v4/channels/channel":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"app.channel.get.existing.app_error","message":"not found","status_code":404}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	c.ServerURL = srv.URL
	c.UserID = "expected-user"

	if _, outcome := c.probeChannelLifecycle(context.Background(), "channel", true); outcome != lifecycleDeleted {
		t.Fatalf("outcome = %v, want deleted", outcome)
	}
}

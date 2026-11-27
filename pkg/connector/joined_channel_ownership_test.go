package connector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
)

// channelServer answers the single-channel fetch with the given channel.
func channelServer(t *testing.T, ch map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(ch)
	}))
}

func ownedClient(serverURL, teamID string) *MattermostClient {
	c := &MattermostClient{Token: "tok", UserID: "me", baseURL: serverURL,
		UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}}}
	c.UserLogin.Metadata = &UserLoginMetadata{TeamID: teamID}
	return c
}

// Joining a channel must not bridge it when another login owns its team: both
// logins share one Mattermost account, so both are told about every join. The
// channel must also stay out of the per-login channel cache, because the
// lifecycle poll judges what is in it.
func TestJoinedChannelResyncIgnoresAnotherTeamsChannel(t *testing.T) {
	srv := channelServer(t, map[string]any{"id": "c1", "team_id": "team-b", "delete_at": 0})
	defer srv.Close()

	c := ownedClient(srv.URL, "team-a")
	if rs := c.joinedChannelResync("c1"); rs != nil {
		t.Fatalf("resync queued for another team's channel: %#v", rs)
	}
	if ch, ok := c.channels["c1"]; ok {
		t.Fatalf("cached another team's channel: %#v", ch)
	}
}

func TestJoinedChannelResyncKeepsItsOwnTeam(t *testing.T) {
	srv := channelServer(t, map[string]any{"id": "c1", "team_id": "team-a", "delete_at": 0})
	defer srv.Close()

	c := ownedClient(srv.URL, "team-a")
	if rs := c.joinedChannelResync("c1"); rs == nil {
		t.Fatal("no resync for this login's own channel")
	}
	if _, ok := c.channels["c1"]; !ok {
		t.Fatal("own channel was not cached")
	}
}

// A direct message has no team, so it is owned by the login that bridges DMs.
func TestJoinedChannelResyncFollowsDMOwnership(t *testing.T) {
	srv := channelServer(t, map[string]any{"id": "d1", "type": "D", "name": "a__b"})
	defer srv.Close()

	without := ownedClient(srv.URL, "team-a")
	if rs := without.joinedChannelResync("d1"); rs != nil {
		t.Fatal("a login without DMs bridged a DM")
	}

	with := ownedClient(srv.URL, "team-a")
	with.UserLogin.Metadata = &UserLoginMetadata{TeamID: "team-a", DMs: true}
	if rs := with.joinedChannelResync("d1"); rs == nil {
		t.Fatal("the DM-owning login did not bridge the DM")
	}
}

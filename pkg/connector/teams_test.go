package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

func TestTeamPortalIDs(t *testing.T) {
	if id, ok := teamIDFromPortal(teamPortalID("abc")); !ok || id != "abc" {
		t.Fatalf("round trip: %q %v", id, ok)
	}
	for _, p := range []networkid.PortalID{"abc", "team:", "", "teamabc", "xteam:abc"} {
		if isTeamPortal(p) {
			t.Errorf("%q treated as a team space", p)
		}
	}
}

func TestChannelParent(t *testing.T) {
	cases := []struct {
		ch   *mattermost.Channel
		want string
	}{
		{&mattermost.Channel{ID: "c", TeamID: "t1", Type: "O"}, "team:t1"},
		{&mattermost.Channel{ID: "c", TeamID: "t1", Type: "P"}, "team:t1"},
		{&mattermost.Channel{ID: "d", Type: "D", Name: "a__b"}, ""},
		{&mattermost.Channel{ID: "g", Type: "G"}, ""},
		{&mattermost.Channel{ID: "d2", TeamID: "t1", Type: "D", Name: "a__b"}, ""},
		{&mattermost.Channel{ID: "g2", TeamID: "t1", Type: "G"}, ""},
		{&mattermost.Channel{ID: "x", Type: "O"}, ""},
		{nil, ""},
	}
	for _, tc := range cases {
		got := channelParent(tc.ch)
		if (got == nil) != (tc.want == "") || (got != nil && string(*got) != tc.want) {
			t.Errorf("channelParent(%+v) = %v, want %q", tc.ch, got, tc.want)
		}
	}
}

func teamClient(srvURL string) *MattermostClient {
	return &MattermostClient{
		UserID:    "me",
		Token:     "tok",
		ServerURL: srvURL,
		UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}},
	}
}

func TestTeamSpaceInfoAndIcon(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte("PNGDATA"))
	}))
	defer srv.Close()
	c := teamClient(srv.URL)
	info := c.teamInfo(&mattermost.Team{ID: "t1", DisplayName: "Team One", Name: "team-one", LastTeamIconUpdate: 42})
	if *info.Name != "Team One" || *info.Type != database.RoomTypeSpace {
		t.Fatalf("name/type = %q %v", *info.Name, *info.Type)
	}
	if info.Members == nil || len(info.Members.MemberMap) != 1 || info.Members.PowerLevels == nil || *info.Members.PowerLevels.EventsDefault != 100 {
		t.Fatalf("members/power = %+v", info.Members)
	}
	if m, ok := info.Members.MemberMap["me"]; !ok || !m.IsFromMe {
		t.Fatalf("self not the member: %+v", info.Members.MemberMap)
	}
	if info.Avatar == nil || info.Avatar.Remove || info.Avatar.ID != "team_t1_42" {
		t.Fatalf("avatar = %+v", info.Avatar)
	}
	data, err := info.Avatar.Get(context.Background())
	if err != nil || string(data) != "PNGDATA" || gotPath != "/api/v4/teams/t1/image" || gotAuth != "Bearer tok" {
		t.Fatalf("icon fetch: %q %v path=%s auth=%s", data, err, gotPath, gotAuth)
	}
	// A new icon gets a new avatar ID, so the space picks it up.
	if c.teamAvatar(&mattermost.Team{ID: "t1", LastTeamIconUpdate: 43}).ID == info.Avatar.ID {
		t.Fatal("icon change keeps the avatar ID")
	}
	// No icon: no image, and a removed one is cleared.
	if a := c.teamAvatar(&mattermost.Team{ID: "t1"}); !a.Remove {
		t.Fatalf("no icon avatar = %+v", a)
	}
	// No display name: the team handle.
	if n := *c.teamInfo(&mattermost.Team{ID: "t2", Name: "handle"}).Name; n != "handle" {
		t.Fatalf("fallback name = %q", n)
	}
}

func TestGetChatInfoForTeamSpace(t *testing.T) {
	c := teamClient("http://unused")
	c.getTeamFn = func(id string) (*mattermost.Team, error) {
		return &mattermost.Team{ID: id, DisplayName: "Team " + id}, nil
	}
	info, err := c.GetChatInfo(context.Background(), &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: teamPortalID("t9")}}})
	if err != nil || *info.Name != "Team t9" || *info.Type != database.RoomTypeSpace {
		t.Fatalf("info = %+v err=%v", info, err)
	}
}

func TestChannelResyncHasTeamParent(t *testing.T) {
	c, _ := dmClient("off")
	c.teamSpacesFn = func() bool { return true }
	ch := &mattermost.Channel{ID: "c1", TeamID: "t1", Type: "O", DisplayName: "Town Square"}
	info, err := c.channelResyncEvent(ch).GetChatInfoFunc(context.Background(), nil)
	if err != nil || info.ParentID == nil || *info.ParentID != "team:t1" {
		t.Fatalf("parent = %v err=%v", info.ParentID, err)
	}
	dm := &mattermost.Channel{ID: "dm1", Type: "D", Name: dmSelfID + "__" + dmOtherID}
	info, _ = c.channelResyncEvent(dm).GetChatInfoFunc(context.Background(), nil)
	if info.ParentID != nil {
		t.Fatalf("DM got a parent %v", *info.ParentID)
	}
}

func TestTeamSpacesNeverTreatedAsChannels(t *testing.T) {
	space := &database.Portal{PortalKey: networkid.PortalKey{ID: teamPortalID("t1"), Receiver: "login"}, MXID: "!space"}
	ch := &database.Portal{PortalKey: networkid.PortalKey{ID: "gone", Receiver: "login"}, MXID: "!room"}
	if ids := unlistedPortalIDs([]*database.Portal{space, ch}, "login", map[string]bool{}); len(ids) != 1 || ids[0] != "gone" {
		t.Fatalf("unlisted = %v; a team space must never be probed for deletion", ids)
	}
	c, views, _ := readClient()
	r := receipt("team:t1", dmNow, "", false)
	if err := c.HandleMatrixReadReceipt(context.Background(), r); err != nil || len(*views) != 0 {
		t.Fatalf("read receipt in a space viewed %v (err %v)", *views, err)
	}
	resp, err := c.FetchMessages(context.Background(), bridgev2.FetchMessagesParams{Portal: &bridgev2.Portal{Portal: space}})
	if err != nil || resp == nil || len(resp.Messages) != 0 {
		t.Fatalf("space backfill = %+v %v", resp, err)
	}
	if err := c.HandleMatrixTyping(context.Background(), &bridgev2.MatrixTyping{Portal: &bridgev2.Portal{Portal: space}, IsTyping: true}); err != nil {
		t.Fatal(err)
	}
}

func TestTeamUpdatesQueueSpaceResync(t *testing.T) {
	p := newProcessEventsHarness(t, lifecycleOptions{})
	p.client.teamSpacesFn = func() bool { return true }
	p.deliver(&mattermost.TeamUpdatedEvent{Team: &mattermost.Team{ID: "t1", DisplayName: "Renamed"}})
	evs := p.events.drain()
	if len(evs) != 1 {
		t.Fatalf("queued %d events", len(evs))
	}
	rs, ok := evs[0].(*simplevent.ChatResync)
	if !ok || rs.PortalKey.ID != "team:t1" || !rs.CreatePortal || *rs.ChatInfo.Name != "Renamed" {
		t.Fatalf("event = %#v", evs[0])
	}
}

func TestSyncCreatesTeamSpacesBeforeChannels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me/teams"):
			_ = json.NewEncoder(w).Encode([]*mattermost.Team{{ID: "t1", DisplayName: "One"}, {ID: "t2", DisplayName: "Two"}})
		case strings.HasSuffix(r.URL.Path, "/teams/t1/channels"):
			_ = json.NewEncoder(w).Encode([]*mattermost.Channel{{ID: "c1", TeamID: "t1", Type: "O", DisplayName: "Town Square"}})
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
	defer srv.Close()
	c := teamClient(srv.URL)
	c.teamSpacesFn = func() bool { return true }
	var mu sync.Mutex
	var keys []string
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		if ev.GetType() == bridgev2.RemoteEventChatResync {
			mu.Lock()
			keys = append(keys, string(ev.GetPortalKey().ID))
			mu.Unlock()
		}
		return bridgev2.EventHandlingResultQueued
	}
	c.syncChannels()
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(keys, ",") != "team:t1,team:t2,c1" {
		t.Fatalf("resyncs = %v, want both spaces before the channel", keys)
	}
}

func TestTeamSpacesOffMovesChannelsOutAndDeletesSpaces(t *testing.T) {
	c, _ := dmClient("off")
	c.teamSpacesFn = func() bool { return false }
	ch := &mattermost.Channel{ID: "c1", TeamID: "t1", Type: "O", DisplayName: "Town Square"}
	info, _ := c.channelResyncEvent(ch).GetChatInfoFunc(context.Background(), nil)
	if info.ParentID == nil || *info.ParentID != "" {
		t.Fatalf("parent = %v, want explicitly none (moves the room out of a team space)", info.ParentID)
	}
	dm := &mattermost.Channel{ID: "dm1", Type: "D", Name: dmSelfID + "__" + dmOtherID}
	if info, _ := c.channelResyncEvent(dm).GetChatInfoFunc(context.Background(), nil); info.ParentID != nil {
		t.Fatalf("DM parent = %v, want untouched", *info.ParentID)
	}

	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return bridgev2.EventHandlingResultQueued
	}
	c.spaceChildrenFn = func(string) bool { return false }
	c.syncTeams([]*mattermost.Team{{ID: "t1"}, {ID: "t2"}})
	c.queueTeamResync(&mattermost.Team{ID: "t1"}) // e.g. update_team: nothing with spaces off
	if len(queued) != 2 {
		t.Fatalf("queued %d events, want two space deletions", len(queued))
	}
	for i, want := range []string{"team:t1", "team:t2"} {
		if queued[i].GetType() != bridgev2.RemoteEventChatDelete || string(queued[i].GetPortalKey().ID) != want {
			t.Fatalf("event %d = %v %v, want delete of %s", i, queued[i].GetType(), queued[i].GetPortalKey(), want)
		}
	}
}

func TestTeamSpacesConfig(t *testing.T) {
	c := teamClient("http://unused")
	if c.teamSpacesEnabled() {
		t.Fatal("team spaces on without config")
	}
	c.UserLogin.Bridge = &bridgev2.Bridge{Network: &MattermostConnector{Config: &Config{TeamSpaces: true}}}
	if !c.teamSpacesEnabled() {
		t.Fatal("team_spaces: true ignored")
	}
}

func TestGetChatInfoFollowsTeamSpacesSetting(t *testing.T) {
	for _, on := range []bool{true, false} {
		c, _ := dmClient("off")
		c.baseURL = "http://127.0.0.1:1" // fresh fetch fails; the cached channel is used
		c.channels["c1"] = &mattermost.Channel{ID: "c1", TeamID: "t1", Type: "O", DisplayName: "Town Square"}
		c.teamSpacesFn = func() bool { return on }
		info, err := c.GetChatInfo(context.Background(), &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "c1"}}})
		want := ""
		if on {
			want = "team:t1"
		}
		if err != nil || info.ParentID == nil || string(*info.ParentID) != want {
			t.Fatalf("team_spaces=%v: parent = %v err=%v, want %q", on, info.ParentID, err, want)
		}
	}
}

func TestSpaceWithRoomsInsideIsNotDeleted(t *testing.T) {
	c := teamClient("http://unused")
	c.teamSpacesFn = func() bool { return false }
	c.spaceChildrenFn = func(id string) bool { return id == "t1" }
	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return bridgev2.EventHandlingResultQueued
	}
	c.syncTeams([]*mattermost.Team{{ID: "t1"}, {ID: "t2"}})
	if len(queued) != 1 || queued[0].GetPortalKey().ID != "team:t2" {
		t.Fatalf("queued %v, want only the empty space t2 deleted", queued)
	}
	// No database to ask: delete nothing.
	c.spaceChildrenFn = nil
	queued = nil
	c.syncTeams([]*mattermost.Team{{ID: "t3"}})
	if len(queued) != 0 {
		t.Fatalf("deleted %v without knowing whether rooms are inside", queued)
	}
}

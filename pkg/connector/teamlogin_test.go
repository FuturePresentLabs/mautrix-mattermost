package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
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

const tlServer = "https://mm.example.com"

func tlLogin(id string, meta *UserLoginMetadata) *bridgev2.UserLogin {
	if meta.ServerURL == "" {
		meta.ServerURL = tlServer
	}
	if meta.UserID == "" {
		meta.UserID = "me"
	}
	return &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: networkid.UserLoginID(id), Metadata: meta}}
}

var tlTeams = []*mattermost.Team{
	{ID: "tB", Name: "friends", DisplayName: "Friends of Example", LastTeamIconUpdate: 7},
	{ID: "tA", Name: "example", DisplayName: "Example"},
}

func TestTeamLabelsAndChoice(t *testing.T) {
	dup := []*mattermost.Team{{ID: "1", Name: "a", DisplayName: "Same"}, {ID: "2", Name: "b", DisplayName: "Same"}, {ID: "3", Name: "c"}}
	if got := teamOptions(dup); strings.Join(got, "|") != "Same (a)|Same (b)|c" {
		t.Fatalf("options = %v", got)
	}
	for choice, want := range map[string]string{"Same (b)": "2", "same (A)": "1", "c": "3", "2": "2", " b ": "2"} {
		if got := teamForOption(dup, choice); got == nil || got.ID != want {
			t.Errorf("choice %q -> %v, want %s", choice, got, want)
		}
	}
	if teamForOption(dup, "Same") != nil {
		t.Error("ambiguous display name accepted")
	}
}

func TestLegacyLoginGetsFirstTeamByName(t *testing.T) {
	if got := teamForLegacyLogin(tlTeams); got.ID != "tA" {
		t.Fatalf("legacy team = %s, want the alphabetically first (Example)", got.ID)
	}
	if teamForLegacyLogin(nil) != nil {
		t.Fatal("team from nothing")
	}
}

func TestTeamLoginIDs(t *testing.T) {
	legacy := tlLogin("me", &UserLoginMetadata{})
	pinned := tlLogin("custom-id", &UserLoginMetadata{TeamID: "tB"})
	other := tlLogin("you_tA", &UserLoginMetadata{UserID: "you", TeamID: "tA"})
	if id := teamLoginID([]*bridgev2.UserLogin{pinned, other}, tlServer, "me", "tB"); id != "custom-id" {
		t.Fatalf("relogin to a pinned team = %s", id)
	}
	if id := teamLoginID([]*bridgev2.UserLogin{legacy, other}, tlServer+"/", "me", "tA"); id != "me" {
		t.Fatalf("an unpinned older login is reused, got %s", id)
	}
	if id := teamLoginID([]*bridgev2.UserLogin{pinned, other}, tlServer, "me", "tA"); id != "me_tA" {
		t.Fatalf("new team = %s", id)
	}
	elsewhere := tlLogin("elsewhere", &UserLoginMetadata{ServerURL: "https://other.example.com", TeamID: "tA"})
	if id := teamLoginID([]*bridgev2.UserLogin{elsewhere}, tlServer, "me", "tA"); id != "me_tA" {
		t.Fatalf("a login on another server was reused: %s", id)
	}
}

func TestFirstLoginGetsTheDMs(t *testing.T) {
	if !dmOwnerFor(nil, "me_tA", tlServer, "me") {
		t.Fatal("first login does not get the DMs")
	}
	owner := tlLogin("me_tA", &UserLoginMetadata{TeamID: "tA", DMs: true})
	if dmOwnerFor([]*bridgev2.UserLogin{owner}, "me_tB", tlServer, "me") {
		t.Fatal("second login took the DMs")
	}
	if !dmOwnerFor([]*bridgev2.UserLogin{owner}, "me_tA", tlServer, "me") {
		t.Fatal("relogin of the DM login lost the DMs")
	}
	legacy := tlLogin("me", &UserLoginMetadata{})
	if dmOwnerFor([]*bridgev2.UserLogin{legacy}, "me_tB", tlServer, "me") {
		t.Fatal("an unpinned older login carries the DMs; a new login must not")
	}
	otherAccount := tlLogin("you_tA", &UserLoginMetadata{UserID: "you", TeamID: "tA", DMs: true})
	if !dmOwnerFor([]*bridgev2.UserLogin{otherAccount}, "me_tA", tlServer, "me") {
		t.Fatal("another account's DM login counted")
	}
}

func scopedClient(meta *UserLoginMetadata) *MattermostClient {
	l := tlLogin("me_"+meta.TeamID, meta)
	return &MattermostClient{UserLogin: l, UserID: "me", Token: "tok"}
}

func TestChannelOwnership(t *testing.T) {
	chA := &mattermost.Channel{ID: "a", TeamID: "tA", Type: "O"}
	chB := &mattermost.Channel{ID: "b", TeamID: "tB", Type: "P"}
	dm := &mattermost.Channel{ID: "d", Type: "D", Name: "me__x"}
	gm := &mattermost.Channel{ID: "g", Type: "G"}
	cases := []struct {
		name               string
		meta               *UserLoginMetadata
		a, b, d, g, posted bool
	}{
		{"team A with DMs", &UserLoginMetadata{TeamID: "tA", DMs: true}, true, false, true, true, true},
		{"team B without DMs", &UserLoginMetadata{TeamID: "tB"}, false, true, false, false, false},
		{"not yet pinned", &UserLoginMetadata{}, true, true, true, true, true},
	}
	for _, tc := range cases {
		c := scopedClient(tc.meta)
		got := []bool{c.ownsChannel(chA), c.ownsChannel(chB), c.ownsChannel(dm), c.ownsChannel(gm), c.ownsPostedChannel("D", "")}
		want := []bool{tc.a, tc.b, tc.d, tc.g, tc.posted}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: ownership %v, want %v", tc.name, got, want)
				break
			}
		}
	}
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	if !c.ownsPostedChannel("O", "tA") || c.ownsPostedChannel("P", "tB") || c.ownsPostedChannel("G", "") {
		t.Fatal("posted ownership wrong")
	}
	if !c.ownsPost(&mattermost.PostedEvent{}) {
		t.Fatal("a post without channel metadata is dropped")
	}
	if c.ownsChannel(nil) {
		t.Fatal("nil channel owned")
	}
}

// teamLoginServer lists tlTeams; tA has a channel and a DM, tB a channel
// and the same DM.
func teamLoginServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dm := &mattermost.Channel{ID: "dm1", Type: "D", Name: "me__x"}
		switch {
		case strings.HasSuffix(r.URL.Path, "/users/me/teams"):
			_ = json.NewEncoder(w).Encode(tlTeams)
		case strings.HasSuffix(r.URL.Path, "/channels") && strings.Contains(r.URL.Path, "/users/"):
			// The lifecycle poll lists every channel the user belongs to in one
			// call, archived ones included, and filters by team ownership.
			_ = json.NewEncoder(w).Encode([]*mattermost.Channel{
				{ID: "cA", TeamID: "tA", Type: "O", DisplayName: "Town Square"},
				{ID: "cB", TeamID: "tB", Type: "O", DisplayName: "Town Square"},
				dm,
			})
		case strings.HasSuffix(r.URL.Path, "/teams/tA/channels"):
			_ = json.NewEncoder(w).Encode([]*mattermost.Channel{{ID: "cA", TeamID: "tA", Type: "O", DisplayName: "Town Square"}, dm})
		case strings.HasSuffix(r.URL.Path, "/teams/tB/channels"):
			_ = json.NewEncoder(w).Encode([]*mattermost.Channel{{ID: "cB", TeamID: "tB", Type: "O", DisplayName: "Town Square"}, dm})
		default:
			_, _ = w.Write([]byte("[]"))
		}
	}))
}

type syncRecord struct {
	mu       sync.Mutex
	resynced []string
	deleted  []string
	saved    int
	state    []string
	uploads  int
}

func syncedClient(t *testing.T, meta *UserLoginMetadata, portals map[string]bool) (*MattermostClient, *syncRecord) {
	t.Helper()
	srv := teamLoginServer()
	t.Cleanup(srv.Close)
	c := scopedClient(meta)
	c.baseURL = srv.URL
	c.channelMembersFn = func(string) ([]*mattermost.ChannelMember, error) { return nil, nil }
	c.usersByIDsFn = func([]string) ([]*mattermost.User, error) { return nil, nil }
	rec := &syncRecord{}
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		switch ev.GetType() {
		case bridgev2.RemoteEventChatResync:
			rec.resynced = append(rec.resynced, string(ev.GetPortalKey().ID))
		case bridgev2.RemoteEventChatDelete:
			rec.deleted = append(rec.deleted, string(ev.GetPortalKey().ID))
		}
		return bridgev2.EventHandlingResultQueued
	}
	c.hasPortalFn = func(id string) bool { return portals[id] }
	c.saveLoginFn = func() { rec.mu.Lock(); rec.saved++; rec.mu.Unlock() }
	c.setSpaceStateFn = func(typ event.Type, content any) {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		switch v := content.(type) {
		case *event.RoomNameEventContent:
			rec.state = append(rec.state, "name="+v.Name)
		case *event.RoomAvatarEventContent:
			rec.state = append(rec.state, "avatar="+string(v.URL))
		}
	}
	c.uploadTeamIconFn = func(team *mattermost.Team) (id.ContentURIString, error) {
		rec.mu.Lock()
		rec.uploads++
		rec.mu.Unlock()
		return id.ContentURIString("mxc://example.com/" + team.ID), nil
	}
	c.spaceChildrenFn = func(string) bool { return false }
	return c, rec
}

func TestSyncBridgesOnlyOwnChannels(t *testing.T) {
	// Team B login without DMs, holding stale rooms for team A's channel and the DM.
	c, rec := syncedClient(t, &UserLoginMetadata{TeamID: "tB", TeamName: "Friends of Example"}, map[string]bool{"cA": true, "dm1": true})
	c.syncChannels()
	if strings.Join(rec.resynced, ",") != "cB" {
		t.Fatalf("resynced %v, want only cB", rec.resynced)
	}
	got := map[string]bool{}
	for _, d := range withoutSpaces(rec.deleted) {
		got[d] = true
	}
	if !got["cA"] || !got["dm1"] || got["cB"] {
		t.Fatalf("deleted %v, want the stale cA and dm1 rooms only", rec.deleted)
	}
	if _, ok := c.channels["cA"]; ok {
		t.Fatal("another team's channel cached")
	}
}

func TestSyncWithDMs(t *testing.T) {
	c, rec := syncedClient(t, &UserLoginMetadata{TeamID: "tA", TeamName: "Example", DMs: true}, nil)
	c.syncChannels()
	if strings.Join(sorted(rec.resynced), ",") != "cA,dm1" {
		t.Fatalf("resynced %v, want cA and the DM once", rec.resynced)
	}
	if d := withoutSpaces(rec.deleted); len(d) != 0 {
		t.Fatalf("deleted %v; no stale rooms", d)
	}
}

func TestLegacyLoginIsPinnedOnSync(t *testing.T) {
	meta := &UserLoginMetadata{}
	c, rec := syncedClient(t, meta, map[string]bool{"cB": true})
	c.syncChannels()
	if meta.TeamID != "tA" || !meta.DMs || meta.TeamName != "Example" {
		t.Fatalf("pinned to %+v, want team tA with DMs", meta)
	}
	if strings.Join(sorted(rec.resynced), ",") != "cA,dm1" || strings.Join(withoutSpaces(rec.deleted), ",") != "cB" {
		t.Fatalf("resynced %v deleted %v", rec.resynced, rec.deleted)
	}
	if rec.saved == 0 {
		t.Fatal("pinned login not saved")
	}
}

func TestLoginProfileFollowsTeam(t *testing.T) {
	meta := &UserLoginMetadata{TeamID: "tB", Username: "me-user"}
	c, rec := syncedClient(t, meta, nil)
	c.syncChannels()
	ul := c.UserLogin
	if ul.RemoteName != "Friends of Example" || ul.RemoteProfile.Name != "Friends of Example" || ul.RemoteProfile.Username != "me-user" {
		t.Fatalf("profile = %q %+v", ul.RemoteName, ul.RemoteProfile)
	}
	if ul.RemoteProfile.Avatar != "mxc://example.com/tB" || meta.TeamIcon != "tB_7" || rec.uploads != 1 {
		t.Fatalf("avatar %q icon %q uploads %d", ul.RemoteProfile.Avatar, meta.TeamIcon, rec.uploads)
	}
	if !strings.Contains(strings.Join(rec.state, ";"), "avatar=mxc://example.com/tB") || !strings.Contains(strings.Join(rec.state, ";"), "name=") {
		t.Fatalf("space state = %v", rec.state)
	}
	// Unchanged icon: no second upload.
	rec.state = nil
	c.syncChannels()
	if rec.uploads != 1 || len(rec.state) != 0 {
		t.Fatalf("resync re-uploaded (%d) or rewrote the space (%v)", rec.uploads, rec.state)
	}
}

func TestPostedEventsGatedByTeam(t *testing.T) {
	p := newProcessEventsHarness(t, lifecycleOptions{})
	p.client.UserLogin.Metadata = &UserLoginMetadata{TeamID: "tB"}
	post := func(id, typ, team string) *mattermost.PostedEvent {
		return &mattermost.PostedEvent{Post: &mattermost.Post{ID: id, ChannelID: "ch-" + id, UserID: "x", Message: "hi"}, ChannelID: "ch-" + id, ChannelType: typ, TeamID: team, ChannelName: "n"}
	}
	p.deliver(post("1", "O", "tA"), post("2", "D", ""), post("3", "O", "tB"), &mattermost.DirectAddedEvent{ChannelID: "dmX"},
		&mattermost.ChannelCreatedEvent{Channel: &mattermost.Channel{ID: "newA", TeamID: "tA", Type: "O"}})
	var msgs []string
	for _, ev := range p.events.drain() {
		if ev.GetType() == bridgev2.RemoteEventMessage {
			msgs = append(msgs, string(ev.GetPortalKey().ID))
		}
		if ev.GetType() == bridgev2.RemoteEventChatResync {
			t.Fatalf("resync for %s; neither the other team's channel nor a DM belongs here", ev.GetPortalKey().ID)
		}
	}
	if strings.Join(msgs, ",") != "ch-3" {
		t.Fatalf("messages bridged for %v, want only ch-3", msgs)
	}
}

func TestChooseDMLogin(t *testing.T) {
	a := tlLogin("me_tA", &UserLoginMetadata{TeamID: "tA", TeamName: "Example", DMs: true})
	b := tlLogin("me_tB", &UserLoginMetadata{TeamID: "tB", TeamName: "Friends of Example"})
	other := tlLogin("you_tC", &UserLoginMetadata{UserID: "you", TeamID: "tC", TeamName: "Other", DMs: true})
	logins := []*bridgev2.UserLogin{a, b}
	target, changed, err := chooseDMLogin(logins, "friends of example")
	if err != nil || target != b || len(changed) != 2 {
		t.Fatalf("target %v changed %d err %v", target, len(changed), err)
	}
	if a.Metadata.(*UserLoginMetadata).DMs || !b.Metadata.(*UserLoginMetadata).DMs {
		t.Fatal("DMs not moved")
	}
	if _, changed, _ := chooseDMLogin(logins, "tB"); len(changed) != 0 {
		t.Fatal("no-op change reported")
	}
	if _, _, err := chooseDMLogin(logins, "nope"); err == nil || !strings.Contains(err.Error(), "Example") {
		t.Fatalf("unknown team error = %v", err)
	}
	// Another account's logins are left alone.
	if _, changed, _ := chooseDMLogin([]*bridgev2.UserLogin{a, b, other}, "Example"); len(changed) != 2 || !other.Metadata.(*UserLoginMetadata).DMs {
		t.Fatalf("changed %d; other account touched: %v", len(changed), other.Metadata.(*UserLoginMetadata).DMs)
	}
	// Two accounts' logins with the same team name: refuse rather than guess.
	twin := tlLogin("you_tA", &UserLoginMetadata{UserID: "you", TeamID: "tA2", TeamName: "Example"})
	if _, _, err := chooseDMLogin([]*bridgev2.UserLogin{a, twin}, "Example"); err == nil {
		t.Fatal("ambiguous team name accepted")
	}
	if !strings.Contains(describeDMTeam([]*bridgev2.UserLogin{a, b}), "Example (DMs and group DMs)") {
		t.Fatalf("description:\n%s", describeDMTeam([]*bridgev2.UserLogin{a, b}))
	}
}

func TestTeamStepAsksWhenSeveralTeams(t *testing.T) {
	srv := teamLoginServer()
	defer srv.Close()
	var s teamStep
	step, err := s.begin(context.Background(), nil, srv.URL, "tok", "me", "me-user")
	if err != nil || step.Type != bridgev2.LoginStepTypeUserInput || !s.pending() {
		t.Fatalf("step %+v err %v", step, err)
	}
	f := step.UserInputParams.Fields[0]
	if f.Type != bridgev2.LoginInputFieldTypeSelect || strings.Join(f.Options, "|") != "Friends of Example|Example" {
		t.Fatalf("field = %+v", f)
	}
	if _, err := s.submit(context.Background(), nil, map[string]string{teamFieldID: "nope"}); err == nil || !s.pending() {
		t.Fatalf("unknown team accepted (err %v)", err)
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// withoutSpaces drops team space deletions (team spaces are off here).
func withoutSpaces(in []string) []string {
	var out []string
	for _, s := range in {
		if !strings.HasPrefix(s, teamPortalPrefix) {
			out = append(out, s)
		}
	}
	return out
}

func TestTeamStepSingleTeamFinishesDirectly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*mattermost.Team{{ID: "only", DisplayName: "Only"}})
	}))
	defer srv.Close()
	var finished *mattermost.Team
	s := teamStep{finishFn: func(v *verifiedLogin, team *mattermost.Team) (*bridgev2.LoginStep, error) {
		finished = team
		return &bridgev2.LoginStep{Type: bridgev2.LoginStepTypeComplete}, nil
	}}
	step, err := s.begin(context.Background(), nil, srv.URL, "tok", "me", "me-user")
	if err != nil || step.Type != bridgev2.LoginStepTypeComplete || finished == nil || finished.ID != "only" || s.pending() {
		t.Fatalf("step %+v err %v finished %v", step, err, finished)
	}
	// Two teams: the chosen one finishes.
	srv2 := teamLoginServer()
	defer srv2.Close()
	s2 := teamStep{finishFn: s.finishFn}
	if _, err := s2.begin(context.Background(), nil, srv2.URL, "tok", "me", "me-user"); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.submit(context.Background(), nil, map[string]string{teamFieldID: "Example"}); err != nil || finished.ID != "tA" || s2.pending() {
		t.Fatalf("submit: err %v finished %v", err, finished)
	}
}

func TestPinSavesLogin(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{})
	saved := 0
	c.saveLoginFn = func() { saved++ }
	c.pinLegacyLogin(tlTeams)
	if saved != 1 {
		t.Fatalf("saved %d times, want 1", saved)
	}
	c.pinLegacyLogin(tlTeams) // already pinned: nothing to do
	if saved != 1 {
		t.Fatal("re-pinned a pinned login")
	}
}

func TestOnlyOwnChannelsGetRooms(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{TeamID: "tB"})
	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return bridgev2.EventHandlingResultQueued
	}
	c.queueChannelResync(&mattermost.Channel{ID: "cA", TeamID: "tA", Type: "O"})
	c.queueChannelResync(&mattermost.Channel{ID: "dm", Type: "D", Name: "me__x"})
	if len(queued) != 0 {
		t.Fatalf("rooms queued for other logins' channels: %v", queued)
	}
	c.queueChannelResync(&mattermost.Channel{ID: "cB", TeamID: "tB", Type: "O"})
	if len(queued) != 1 {
		t.Fatal("own channel not resynced")
	}
}

func TestLifecyclePollListsOnlyOwnChannels(t *testing.T) {
	srv := teamLoginServer()
	defer srv.Close()
	c := scopedClient(&UserLoginMetadata{TeamID: "tB"})
	c.baseURL = srv.URL
	got, ok := c.listChannels(t.Context())
	if !ok || len(got) != 1 || got["cB"] == nil {
		t.Fatalf("listed %v ok=%v, want only cB", got, ok)
	}
}

func TestDirectAddedIgnoredWithoutDMs(t *testing.T) {
	hits := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(&mattermost.Channel{ID: "dmX", Type: "D", Name: "me__x"})
	}))
	defer srv.Close()
	p := newProcessEventsHarness(t, lifecycleOptions{})
	p.client.UserLogin.Metadata = &UserLoginMetadata{TeamID: "tB"}
	p.client.baseURL = srv.URL
	p.deliver(&mattermost.DirectAddedEvent{ChannelID: "dmX"})
	time.Sleep(200 * time.Millisecond) // the handler fetches in a goroutine
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("a login without DMs fetched a new DM (%d requests)", hits)
	}
}

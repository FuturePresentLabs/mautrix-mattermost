package connector

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// accountClient is an account login (no team) in account-login mode.
func accountClient(t *testing.T, enabled map[string]bool, portals map[string]bool) (*MattermostClient, *syncRecord) {
	c, rec := syncedClient(t, &UserLoginMetadata{EnabledTeams: enabled}, portals)
	c.accountLoginsFn = func() bool { return true }
	c.teamSpacesFn = func() bool { return true }
	c.clearSpaceParentsFn = func(string) {}
	return c, rec
}

func TestAccountLoginBridgesSwitchedOnTeams(t *testing.T) {
	c, rec := accountClient(t, map[string]bool{"tA": true}, map[string]bool{"cB": true})
	c.syncChannels()
	if got := strings.Join(sorted(rec.resynced), ","); got != "cA,dm1,team:tA" {
		t.Fatalf("resynced %s, want team A's channel, its space and the DM", got)
	}
	if got := strings.Join(sorted(rec.deleted), ","); got != "cB,team:tB" {
		t.Fatalf("deleted %s, want the switched-off team's room and space", got)
	}
	if c.loginMeta().TeamID != "" {
		t.Fatal("account login pinned to a team")
	}
}

func TestTeamsStartSwitchedOff(t *testing.T) {
	c, rec := accountClient(t, nil, nil)
	c.syncChannels()
	if got := strings.Join(sorted(rec.resynced), ","); got != "dm1" {
		t.Fatalf("resynced %s, want only the DM", got)
	}
}

func TestSpacesOffInAccountMode(t *testing.T) {
	c, rec := accountClient(t, map[string]bool{"tA": true, "tB": true}, nil)
	c.teamSpacesFn = func() bool { return false }
	c.syncChannels()
	if got := strings.Join(sorted(rec.resynced), ","); got != "cA,cB,dm1" {
		t.Fatalf("resynced %s", got)
	}
	if got := strings.Join(sorted(rec.deleted), ","); got != "team:tA,team:tB" {
		t.Fatalf("deleted %s, want both spaces gone", got)
	}
}

func TestAccountOwnership(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{EnabledTeams: map[string]bool{"tA": true}})
	c.accountLoginsFn = func() bool { return true }
	if !c.ownsPostedChannel("O", "tA") || c.ownsPostedChannel("P", "tB") || !c.ownsPostedChannel("D", "") || !c.ownsPostedChannel("G", "") {
		t.Fatal("posted ownership wrong")
	}
	if c.ownsTeam("") {
		t.Fatal("a channel without a team is owned")
	}
	if !c.ownsDMs() {
		t.Fatal("account login does not carry the DMs")
	}
	// The same login outside account mode is a legacy login: everything.
	c.accountLoginsFn = func() bool { return false }
	if !c.ownsTeam("tB") {
		t.Fatal("legacy login lost a team")
	}
}

func TestAccountLoginIsNotPinned(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{})
	c.accountLoginsFn = func() bool { return true }
	c.saveLoginFn = func() { t.Fatal("saved") }
	c.pinLegacyLogin(tlTeams)
	if c.loginMeta().TeamID != "" {
		t.Fatal("pinned")
	}
}

func TestTeamCommand(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{})
	c.accountLoginsFn = func() bool { return true }
	c.listTeamsFn = func() ([]*mattermost.Team, error) { return tlTeams, nil }
	saves, resyncs := 0, 0
	var delays []time.Duration
	c.saveLoginFn = func() { saves++ }
	c.resyncFn = func() { resyncs++ }
	c.afterDelayFn = func(d time.Duration, f func()) { delays = append(delays, d); f() }

	list := c.runTeamCommand(nil)
	if !strings.Contains(list, "* Example: off") || !strings.Contains(list, "* Friends of Example: off") || strings.Index(list, "Example: off") > strings.Index(list, "Friends") {
		t.Fatalf("list:\n%s", list)
	}
	if r := c.runTeamCommand([]string{"on", "friends", "of", "example"}); !strings.Contains(r, "**Friends of Example** is on") {
		t.Fatalf("on: %s", r)
	}
	if !c.loginMeta().EnabledTeams["tB"] || saves != 1 || resyncs != 1 || len(delays) != 0 {
		t.Fatalf("after on: %v saves %d resyncs %d delays %v", c.loginMeta().EnabledTeams, saves, resyncs, delays)
	}
	if r := c.runTeamCommand([]string{"ON", "Friends of Example"}); !strings.Contains(r, "already on") || saves != 1 {
		t.Fatalf("repeat: %s (saves %d)", r, saves)
	}
	if !strings.Contains(c.runTeamCommand(nil), "* Friends of Example: **on**") {
		t.Fatal("list does not show the team on")
	}
	if r := c.runTeamCommand([]string{"off", "tB"}); !strings.Contains(r, "is off") {
		t.Fatalf("off: %s", r)
	}
	if _, on := c.loginMeta().EnabledTeams["tB"]; on || saves != 2 || resyncs != 3 || len(delays) != 1 || delays[0] != spaceCleanupDelay {
		t.Fatalf("after off: %v saves %d resyncs %d delays %v", c.loginMeta().EnabledTeams, saves, resyncs, delays)
	}
	if r := c.runTeamCommand([]string{"off", "Example"}); !strings.Contains(r, "already off") {
		t.Fatalf("off when off: %s", r)
	}
	for _, args := range [][]string{{"maybe", "Example"}, {"on"}} {
		if r := c.runTeamCommand(args); !strings.HasPrefix(r, "Usage") {
			t.Errorf("%v: %s", args, r)
		}
	}
	if r := c.runTeamCommand([]string{"on", "Nope"}); !strings.Contains(r, `No team "Nope"`) {
		t.Fatalf("unknown: %s", r)
	}
	c.listTeamsFn = func() ([]*mattermost.Team, error) { return nil, errors.New("down") }
	if r := c.runTeamCommand(nil); !strings.Contains(r, "Could not list your teams: down") {
		t.Fatalf("error: %s", r)
	}
}

func TestLoginCoversAccountWhenTeamLoginsOff(t *testing.T) {
	srv := teamLoginServer()
	defer srv.Close()
	var finished []*mattermost.Team
	called := false
	s := teamStep{perTeamFn: func() bool { return false }, finishFn: func(v *verifiedLogin, team *mattermost.Team) (*bridgev2.LoginStep, error) {
		called = true
		finished = append(finished, team)
		return &bridgev2.LoginStep{Type: bridgev2.LoginStepTypeComplete}, nil
	}}
	step, err := s.begin(context.Background(), nil, srv.URL, "tok", "me", "me-user")
	if err != nil || step.Type != bridgev2.LoginStepTypeComplete || !called || finished[0] != nil || s.pending() {
		t.Fatalf("step %+v err %v finished %v", step, err, finished)
	}
}

func TestAccountLoginMetadata(t *testing.T) {
	v := &verifiedLogin{serverURL: "https://mm.example.com/", userID: "me", username: "alice", token: "new"}
	id, meta := accountLogin(nil, v)
	if id != "me" || !meta.DMs || meta.TeamID != "" || meta.Token != "new" || meta.EnabledTeams != nil {
		t.Fatalf("new: %s %+v", id, meta)
	}
	teamOne := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "me_tA", Metadata: &UserLoginMetadata{UserID: "me", ServerURL: "https://mm.example.com", TeamID: "tA"}}}
	other := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "x", Metadata: &UserLoginMetadata{UserID: "other", ServerURL: "https://mm.example.com"}}}
	old := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "acct", Metadata: &UserLoginMetadata{UserID: "me", ServerURL: "https://mm.example.com", Token: "old",
		EnabledTeams: map[string]bool{"tA": true}, MutesSynced: true}}}
	id, meta = accountLogin([]*bridgev2.UserLogin{teamOne, other, old}, v)
	if id != "acct" || meta.Token != "new" || !meta.EnabledTeams["tA"] || !meta.MutesSynced || !meta.DMs {
		t.Fatalf("relogin: %s %+v", id, meta)
	}
	if id, _ = accountLogin([]*bridgev2.UserLogin{teamOne, other}, v); id != "me" {
		t.Fatalf("a team login was taken for the account: %s", id)
	}
}

func TestChannelsMutedOnCreate(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{})
	c.channels = map[string]*mattermost.Channel{
		"cA":  {ID: "cA", TeamID: "tA", Type: "O"},
		"cB":  {ID: "cB", TeamID: "tA", Type: "P"},
		"dm1": {ID: "dm1", Type: "D"},
		"gm1": {ID: "gm1", Type: "G", TeamID: "tA"},
	}
	exists := map[string]bool{"cB": true}
	c.hasPortalFn = func(id string) bool { return exists[id] }
	on := true
	c.muteChannelsFn = func() bool { return on }
	muted := func(id string) bool {
		ul := c.userLocalInfo(id)
		return ul != nil && ul.MutedUntil != nil && ul.MutedUntil.Equal(event.MutedForever)
	}
	if !muted("cA") || muted("cB") || muted("dm1") || muted("gm1") || muted("unknown") {
		t.Fatalf("mutes: new channel %v, existing room %v, DM %v, group DM %v", muted("cA"), muted("cB"), muted("dm1"), muted("gm1"))
	}
	on = false
	if muted("cA") {
		t.Fatal("muted with the option off")
	}
	c.noteMemberships([]*mattermost.ChannelMember{member("cB", true)})
	if !muted("cB") {
		t.Fatal("a Mattermost mute no longer applies")
	}
}

func TestTeamOptionsFromConfig(t *testing.T) {
	for _, cfg := range []Config{{TeamLogins: true, MuteChannelsOnCreate: false}, {TeamLogins: false, MuteChannelsOnCreate: true}} {
		cfg := cfg
		br := &bridgev2.Bridge{Network: &MattermostConnector{Config: &cfg}}
		c := &MattermostClient{UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{}, Bridge: br}}
		if c.accountLogins() == cfg.TeamLogins || c.muteChannelsOnCreate() != cfg.MuteChannelsOnCreate {
			t.Errorf("%+v: account %v mute %v", cfg, c.accountLogins(), c.muteChannelsOnCreate())
		}
		var s teamStep
		if s.perTeam(&bridgev2.User{Bridge: br}) != cfg.TeamLogins {
			t.Errorf("%+v: login step per team %v", cfg, s.perTeam(&bridgev2.User{Bridge: br}))
		}
	}
	bare := &MattermostClient{}
	var s teamStep
	if bare.accountLogins() || bare.muteChannelsOnCreate() || !s.perTeam(nil) {
		t.Fatal("defaults without a bridge config changed")
	}
}

func TestAccountSpaceName(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{})
	c.accountLoginsFn = func() bool { return true }
	name := "Mattermost DMs"
	c.accountSpaceNameFn = func() string { return name }
	var set []string
	c.setSpaceStateFn = func(typ event.Type, content any) {
		if v, ok := content.(*event.RoomNameEventContent); ok && typ == event.StateRoomName {
			set = append(set, v.Name)
		}
	}
	saves := 0
	c.saveLoginFn = func() { saves++ }
	c.setSpaceStateFails = true
	c.syncAccountSpaceName()
	if c.loginMeta().SpaceName != "" || saves != 0 {
		t.Fatal("a failed rename was recorded")
	}
	c.setSpaceStateFails = false
	c.syncAccountSpaceName()
	c.syncAccountSpaceName()
	if strings.Join(set, "|") != "Mattermost DMs|Mattermost DMs" || saves != 1 || c.loginMeta().SpaceName != name {
		t.Fatalf("set %v saves %d meta %q", set, saves, c.loginMeta().SpaceName)
	}
	name = "Work chats"
	c.syncAccountSpaceName()
	if set[len(set)-1] != "Work chats" || saves != 2 {
		t.Fatalf("rename on change: %v saves %d", set, saves)
	}
	set = nil
	name = ""
	c.syncAccountSpaceName()
	team := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	team.accountLoginsFn = func() bool { return true }
	team.accountSpaceNameFn = func() string { return "X" }
	team.setSpaceStateFn = c.setSpaceStateFn
	team.syncAccountSpaceName()
	perTeam := scopedClient(&UserLoginMetadata{})
	perTeam.accountLoginsFn = func() bool { return false }
	perTeam.accountSpaceNameFn = func() string { return "X" }
	perTeam.setSpaceStateFn = c.setSpaceStateFn
	perTeam.syncAccountSpaceName()
	if len(set) != 0 {
		t.Fatalf("renamed with an empty name, a team login or per-team logins: %v", set)
	}
}

func TestAccountSpaceNameFromConfig(t *testing.T) {
	br := &bridgev2.Bridge{Network: &MattermostConnector{Config: &Config{AccountSpaceName: "  Mattermost DMs "}}}
	c := &MattermostClient{UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{}, Bridge: br}}
	if c.accountSpaceName() != "Mattermost DMs" || (&MattermostClient{}).accountSpaceName() != "" {
		t.Fatalf("name %q", c.accountSpaceName())
	}
}

func TestSyncNamesAccountSpace(t *testing.T) {
	c, rec := accountClient(t, nil, nil)
	c.accountSpaceNameFn = func() string { return "Mattermost DMs" }
	c.syncChannels()
	if strings.Join(rec.state, ",") != "name=Mattermost DMs" {
		t.Fatalf("space state %v", rec.state)
	}
}

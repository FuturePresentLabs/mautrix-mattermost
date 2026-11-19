package connector

import (
	"context"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

func TestChannelTitles(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	town := &mattermost.Channel{ID: "c", TeamID: "tA", Type: "O", DisplayName: "Town Square"}
	dm := &mattermost.Channel{ID: "d", Type: "D", DisplayName: "dm-name"}
	gm := &mattermost.Channel{ID: "g", Type: "G", TeamID: "tA", DisplayName: "alice, bob"}
	if got := c.channelTitle(town); got != "Town Square" {
		t.Fatalf("before teams are known: %q", got)
	}
	c.noteTeams([]*mattermost.Team{{ID: "tA", DisplayName: "Example"}})
	if got := c.channelTitle(town); got != "Town Square" {
		t.Fatalf("one team: %q, want no suffix", got)
	}
	c.noteTeams(tlTeams)
	if got := c.channelTitle(town); got != "Town Square · Example" {
		t.Fatalf("two teams: %q", got)
	}
	if c.channelTitle(dm) != "dm-name" || c.channelTitle(gm) != "alice, bob" {
		t.Fatal("DM or group DM got a team suffix")
	}
	if c.channelTitle(&mattermost.Channel{ID: "x", TeamID: "tZ", Type: "O", DisplayName: "Elsewhere"}) != "Elsewhere" {
		t.Fatal("unknown team suffixed")
	}
	if c.channelTitle(&mattermost.Channel{ID: "e", TeamID: "tA", Type: "O"}) != "" || c.channelTitle(nil) != "" {
		t.Fatal("empty name not empty")
	}
	// In a team space, Beeper shows the space name itself.
	c.teamSpacesFn = func() bool { return true }
	if got := c.channelTitle(town); got != "Town Square" {
		t.Fatalf("with team spaces: %q, want no suffix", got)
	}
}

func TestSyncedChannelNamesCarryTeam(t *testing.T) {
	c, _ := syncedClient(t, &UserLoginMetadata{TeamID: "tB", TeamName: "Friends of Example"}, nil)
	var names []string
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		if rs, ok := ev.(*simplevent.ChatResync); ok && rs.GetChatInfoFunc != nil {
			info, _ := rs.GetChatInfoFunc(context.Background(), nil)
			names = append(names, *info.Name)
		}
		return bridgev2.EventHandlingResultQueued
	}
	c.syncChannels()
	if strings.Join(names, "|") != "Town Square · Friends of Example" {
		t.Fatalf("names = %q", names)
	}
}

func TestChannelRenameKeepsTeam(t *testing.T) {
	p := newProcessEventsHarness(t, lifecycleOptions{})
	p.client.noteTeams(tlTeams)
	p.deliver(&mattermost.ChannelUpdatedEvent{Channel: &mattermost.Channel{ID: "c1", TeamID: "tB", Type: "O", DisplayName: "Renamed"}})
	for _, ev := range p.events.drain() {
		if ch, ok := ev.(*simplevent.ChatInfoChange); ok {
			if got := *ch.ChatInfoChange.ChatInfo.Name; got != "Renamed · Friends of Example" {
				t.Fatalf("rename = %q", got)
			}
			return
		}
	}
	t.Fatal("no name change queued")
}

func TestSpaceUnlinkedBeforeDeletion(t *testing.T) {
	c := teamClient("http://unused")
	c.teamSpacesFn = func() bool { return false }
	c.spaceChildrenFn = func(id string) bool { return id == "busy" }
	var order []string
	c.clearSpaceParentsFn = func(id string) { order = append(order, "unlink:"+id) }
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		order = append(order, "delete:"+string(ev.GetPortalKey().ID))
		return bridgev2.EventHandlingResultQueued
	}
	c.syncTeams([]*mattermost.Team{{ID: "t1"}, {ID: "busy"}})
	if strings.Join(order, ",") != "unlink:t1,delete:team:t1" {
		t.Fatalf("order = %v", order)
	}
}

func TestGetChatInfoNameCarriesTeam(t *testing.T) {
	c, _ := dmClient("off")
	c.baseURL = "http://127.0.0.1:1" // fresh fetch fails; the cached channel is used
	c.channels["c1"] = &mattermost.Channel{ID: "c1", TeamID: "tB", Type: "O", DisplayName: "Town Square"}
	c.noteTeams(tlTeams)
	info, err := c.GetChatInfo(context.Background(), &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "c1"}}})
	if err != nil || info.Name == nil || *info.Name != "Town Square · Friends of Example" {
		t.Fatalf("name = %v err %v", info.Name, err)
	}
}

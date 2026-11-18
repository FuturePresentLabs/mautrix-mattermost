package connector

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

func member(ch string, muted bool) *mattermost.ChannelMember {
	v := "all"
	if muted {
		v = "mention"
	}
	return &mattermost.ChannelMember{ChannelID: ch, UserID: "me", NotifyProps: map[string]string{"mark_unread": v}}
}

type muteLog struct {
	mu    sync.Mutex
	calls []string
	fail  bool
}

func (l *muteLog) mute(ch string, until time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := "unmute"
	if until.After(time.Now()) {
		state = "mute"
	}
	l.calls = append(l.calls, state+":"+ch)
	return !l.fail
}

func (l *muteLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := append([]string(nil), l.calls...)
	sort.Strings(out)
	return strings.Join(out, ",")
}

// mutedClient is a synced client whose Mattermost has cA muted, dm1 not.
func mutedClient(t *testing.T, meta *UserLoginMetadata, members func(string) ([]*mattermost.ChannelMember, error)) (*MattermostClient, *muteLog, *[]bridgev2.RemoteEvent) {
	c, _ := syncedClient(t, meta, map[string]bool{"cA": true, "cB": true, "dm1": true})
	if members == nil {
		members = func(string) ([]*mattermost.ChannelMember, error) {
			return []*mattermost.ChannelMember{member("cA", true), member("cB", true), member("dm1", false)}, nil
		}
	}
	c.membershipsFn = members
	log := &muteLog{}
	c.muteRoomFn = log.mute
	var queued []bridgev2.RemoteEvent
	inner := c.queueRemoteEventFn
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return inner(ev)
	}
	return c, log, &queued
}

func TestNewRoomsStartMutedLikeTheirChannel(t *testing.T) {
	c, _, queued := mutedClient(t, &UserLoginMetadata{TeamID: "tA", DMs: true}, nil)
	c.syncChannels()
	got := map[string]*bridgev2.UserLocalPortalInfo{}
	for _, ev := range *queued {
		if rs, ok := ev.(*simplevent.ChatResync); ok && rs.GetChatInfoFunc != nil {
			info, err := rs.GetChatInfoFunc(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			got[string(rs.PortalKey.ID)] = info.UserLocal
		}
	}
	if ul := got["cA"]; ul == nil || ul.MutedUntil == nil || !ul.MutedUntil.Equal(event.MutedForever) {
		t.Fatalf("cA user-local = %+v, want muted forever", ul)
	}
	if ul, ok := got["dm1"]; !ok || ul != nil {
		t.Fatalf("dm1 user-local = %+v (present %v), want none: an unmuted channel must not unmute the room", ul, ok)
	}
}

func TestGetChatInfoCarriesMute(t *testing.T) {
	c, _ := dmClient("off")
	c.baseURL = "http://127.0.0.1:1"
	c.channels["c1"] = &mattermost.Channel{ID: "c1", TeamID: "tA", Type: "O", DisplayName: "Town Square"}
	c.noteMemberships([]*mattermost.ChannelMember{member("c1", true)})
	info, err := c.GetChatInfo(context.Background(), &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: "c1"}}})
	if err != nil || info.UserLocal == nil || !info.UserLocal.MutedUntil.Equal(event.MutedForever) {
		t.Fatalf("user-local = %+v err %v", info.UserLocal, err)
	}
}

func TestExistingRoomsMutedOnce(t *testing.T) {
	c, log, _ := mutedClient(t, &UserLoginMetadata{TeamID: "tA", DMs: true}, nil)
	savedSynced := false
	c.saveLoginFn = func() {
		if c.loginMeta().MutesSynced {
			savedSynced = true
		}
	}
	c.syncChannels()
	if log.String() != "mute:cA" {
		t.Fatalf("mutes = %q, want only cA (cB is another team's, dm1 is unmuted)", log.String())
	}
	if !c.loginMeta().MutesSynced || !savedSynced {
		t.Fatalf("synced %v, saved with the flag %v", c.loginMeta().MutesSynced, savedSynced)
	}
	c.syncChannels()
	if log.String() != "mute:cA" {
		t.Fatalf("second sync muted again: %q", log.String())
	}
}

func TestExistingMuteSyncRetriedAfterFailures(t *testing.T) {
	failing := func(string) ([]*mattermost.ChannelMember, error) { return nil, errors.New("down") }
	c, log, _ := mutedClient(t, &UserLoginMetadata{TeamID: "tA", DMs: true}, failing)
	c.syncChannels()
	if c.loginMeta().MutesSynced || log.String() != "" {
		t.Fatal("marked synced although mute states could not be fetched")
	}
	c2, log2, _ := mutedClient(t, &UserLoginMetadata{TeamID: "tA", DMs: true}, nil)
	log2.fail = true
	c2.syncChannels()
	if c2.loginMeta().MutesSynced {
		t.Fatal("marked synced although a room could not be muted")
	}
	log2.fail = false
	c2.syncChannels()
	if !c2.loginMeta().MutesSynced {
		t.Fatal("not synced after a clean retry")
	}
}

func TestIncompleteListingDefersMuteSync(t *testing.T) {
	c := scopedClient(&UserLoginMetadata{TeamID: "tA"})
	c.noteMemberships([]*mattermost.ChannelMember{member("cA", true)})
	log := &muteLog{}
	c.muteRoomFn = log.mute
	c.saveLoginFn = func() {}
	c.syncExistingMutes(context.Background(), false)
	if c.loginMeta().MutesSynced || log.String() != "" {
		t.Fatal("ran on an incomplete listing")
	}
}

func TestMattermostMuteChangesMirrored(t *testing.T) {
	c, log, _ := mutedClient(t, &UserLoginMetadata{TeamID: "tA", DMs: true}, nil)
	c.syncChannels()
	log.calls = nil
	ctx := context.Background()
	c.handleMemberUpdated(ctx, member("dm1", true)) // muted in Mattermost
	c.handleMemberUpdated(ctx, member("dm1", true)) // echo: no change
	c.handleMemberUpdated(ctx, member("cA", false)) // unmuted in Mattermost
	c.handleMemberUpdated(ctx, member("cB", false)) // another team's channel
	other := member("dm1", false)
	other.UserID = "someone-else"
	c.handleMemberUpdated(ctx, other) // not our membership
	c.handleMemberUpdated(ctx, nil)
	if log.String() != "mute:dm1,unmute:cA" {
		t.Fatalf("mirrored %q", log.String())
	}
	if m, _ := c.channelMuted("cB"); m {
		t.Fatal("cB state not recorded")
	}
	if m, _ := c.channelMuted("dm1"); !m {
		t.Fatal("other user's membership changed our state")
	}
}

func TestMemberUpdatedEventRouted(t *testing.T) {
	p := newProcessEventsHarness(t, lifecycleOptions{})
	log := &muteLog{}
	p.client.muteRoomFn = log.mute
	p.client.mu.Lock()
	if p.client.channels == nil {
		p.client.channels = map[string]*mattermost.Channel{}
	}
	p.client.channels["c9"] = &mattermost.Channel{ID: "c9", Type: "O"}
	p.client.mu.Unlock()
	m := member("c9", true)
	m.UserID = p.client.UserID
	p.deliver(&mattermost.ChannelMemberUpdatedEvent{Member: m})
	deadline := time.Now().Add(2 * time.Second)
	for log.String() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if log.String() != "mute:c9" {
		t.Fatalf("mirrored %q", log.String())
	}
}

func TestMatrixMutesStayInMatrix(t *testing.T) {
	var c any = &MattermostClient{}
	if _, ok := c.(bridgev2.MuteHandlingNetworkAPI); ok {
		t.Fatal("the connector handles Matrix mutes; they must not reach Mattermost")
	}
}

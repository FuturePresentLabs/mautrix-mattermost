package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

func memberClient(members map[string][]string, fail bool) (*MattermostClient, *eventRecorder) {
	rec := &eventRecorder{}
	c := &MattermostClient{
		Token: "tok", UserID: "me", baseURL: "http://unused.invalid",
		UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}},
		channelMembersFn: func(channelID string) ([]*mattermost.ChannelMember, error) {
			if fail {
				return nil, errors.New("boom")
			}
			var out []*mattermost.ChannelMember
			for _, u := range members[channelID] {
				out = append(out, &mattermost.ChannelMember{ChannelID: channelID, UserID: u})
			}
			return out, nil
		},
	}
	c.queueRemoteEventFn = rec.queue
	return c, rec
}

func memberIDs(l *bridgev2.ChatMemberList) map[string]bool {
	out := map[string]bool{}
	for id, m := range l.MemberMap {
		if m.Membership != event.MembershipJoin {
			continue
		}
		out[string(id)] = true
	}
	return out
}

func TestChatMembers(t *testing.T) {
	ctx := context.Background()
	channel := &mattermost.Channel{ID: "c1", Type: "O", Name: "off-topic"}
	group := &mattermost.Channel{ID: "g1", Type: "G", Name: "group"}
	const meID, otherID = "aaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	dm := &mattermost.Channel{ID: "d1", Type: "D", Name: meID + "__" + otherID}
	c, _ := memberClient(map[string][]string{"c1": {"me", "avery", "jm"}, "g1": {"me", "a", "b"}}, false)

	got := c.chatMembers(ctx, channel)
	if fmt.Sprint(memberIDs(got)) != fmt.Sprint(map[string]bool{"me": true, "avery": true, "jm": true}) {
		t.Fatalf("channel members = %v", memberIDs(got))
	}
	if !got.IsFull || !got.ExcludeChangesFromTimeline {
		t.Fatalf("channel list IsFull=%v Exclude=%v, want both true", got.IsFull, got.ExcludeChangesFromTimeline)
	}
	if !got.MemberMap["me"].IsFromMe || got.MemberMap["me"].SenderLogin != "login" {
		t.Fatalf("own entry is not marked as the logged-in user: %+v", got.MemberMap["me"])
	}

	if g := memberIDs(c.chatMembers(ctx, group)); len(g) != 3 || !g["a"] || !g["b"] {
		t.Fatalf("group DM members = %v", g)
	}

	c.UserID = meID
	c.usersByIDsFn = func(ids []string) ([]*mattermost.User, error) {
		return []*mattermost.User{{ID: otherID, Username: "gh", FirstName: "GitHub"}}, nil
	}
	d := c.chatMembers(ctx, dm)
	if !d.IsFull || d.OtherUserID != otherID || len(memberIDs(d)) != 2 {
		t.Fatalf("DM members = %v other=%q full=%v", memberIDs(d), d.OtherUserID, d.IsFull)
	}
	if info := d.MemberMap[otherID].UserInfo; info == nil || *info.Name != "GitHub" {
		t.Fatalf("DM partner carries no current profile: %+v", info)
	}
}

func TestChatMembersFetchFailureRemovesNobody(t *testing.T) {
	c, _ := memberClient(nil, true)
	got := c.chatMembers(context.Background(), &mattermost.Channel{ID: "c1", Type: "O"})
	if got.IsFull {
		t.Fatal("a failed fetch must not be treated as the full list (it would remove everyone)")
	}
	if ids := memberIDs(got); len(ids) != 1 || !ids["me"] {
		t.Fatalf("members after failure = %v, want only the logged-in user", ids)
	}
}

func TestChatMembersTruncatedPageIsNotFull(t *testing.T) {
	var many []string
	for i := 0; i < channelMembersPageSize; i++ {
		many = append(many, fmt.Sprintf("u%d", i))
	}
	c, _ := memberClient(map[string][]string{"big": many}, false)
	got := c.chatMembers(context.Background(), &mattermost.Channel{ID: "big", Type: "O"})
	if got.IsFull {
		t.Fatal("a page-sized response may be truncated and must not remove members")
	}
	if len(memberIDs(got)) != channelMembersPageSize+1 {
		t.Fatalf("got %d members", len(memberIDs(got)))
	}
}

func TestChannelResyncCarriesMembers(t *testing.T) {
	c, rec := memberClient(map[string][]string{"c1": {"me", "avery"}}, false)
	c.queueChannelResync(&mattermost.Channel{ID: "c1", Type: "O", DisplayName: "Off-Topic"})
	if rec.len() != 1 {
		t.Fatalf("queued %d events", rec.len())
	}
	rs := rec.events[0].(*simplevent.ChatResync)
	info, err := rs.GetChatInfo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids := memberIDs(info.Members); !ids["avery"] || !ids["me"] {
		t.Fatalf("resync members = %v, want the channel's members", ids)
	}
	if info.Name == nil || *info.Name != "Off-Topic" || !info.CanBackfill {
		t.Fatalf("resync lost its name or backfill flag: %+v", info)
	}
}

func TestOwnMembershipEvents(t *testing.T) {
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/channels/c1" {
			fetches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(mattermost.Channel{ID: "c1", Type: "O", DisplayName: "New"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// Being removed must not resync (and so re-create) the channel's room.
	c, rec := memberClient(map[string][]string{"c1": {"me"}}, false)
	c.baseURL = srv.URL
	c.handleChannelMemberEvent(&mattermost.ChannelMemberEvent{ChannelID: "c1", UserID: "me", Added: false})
	time.Sleep(200 * time.Millisecond)
	if rec.len() != 0 || fetches.Load() != 0 {
		t.Fatalf("own removal queued %d events and fetched the channel %d times, want none", rec.len(), fetches.Load())
	}

	// Being added resyncs the channel so its room is created.
	c.handleChannelMemberEvent(&mattermost.ChannelMemberEvent{ChannelID: "c1", UserID: "me", Added: true})
	deadline := time.Now().Add(3 * time.Second)
	for rec.len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rec.len() != 1 {
		t.Fatalf("own addition queued %d events, want 1 resync", rec.len())
	}
	if _, ok := rec.events[0].(*simplevent.ChatResync); !ok {
		t.Fatalf("own addition queued %T, want a ChatResync", rec.events[0])
	}
}

func TestLiveMemberEvents(t *testing.T) {
	c, rec := memberClient(nil, false)
	c.handleChannelMemberEvent(&mattermost.ChannelMemberEvent{ChannelID: "c1", UserID: "avery", Added: true})
	c.handleChannelMemberEvent(&mattermost.ChannelMemberEvent{ChannelID: "c1", UserID: "jm", Added: false})
	if rec.len() != 2 {
		t.Fatalf("queued %d events, want 2", rec.len())
	}
	want := []struct {
		user string
		m    event.Membership
	}{{"avery", event.MembershipJoin}, {"jm", event.MembershipLeave}}
	for i, w := range want {
		ev := rec.events[i].(*simplevent.ChatInfoChange)
		if ev.PortalKey != (networkid.PortalKey{ID: "c1", Receiver: "login"}) {
			t.Fatalf("event %d portal = %+v", i, ev.PortalKey)
		}
		mc := ev.ChatInfoChange.MemberChanges
		if mc == nil || mc.IsFull || mc.ExcludeChangesFromTimeline {
			t.Fatalf("event %d member changes = %+v; live changes must be partial and visible", i, mc)
		}
		if m := mc.MemberMap[networkid.UserID(w.user)]; m.Membership != w.m {
			t.Fatalf("event %d: %s membership = %q, want %q", i, w.user, m.Membership, w.m)
		}
	}
}

func TestChatMembersCarryProfiles(t *testing.T) {
	c, _ := memberClient(map[string][]string{"c1": {"me", "avery", "jm"}}, false)
	var asked []string
	c.usersByIDsFn = func(ids []string) ([]*mattermost.User, error) {
		asked = ids
		return []*mattermost.User{
			{ID: "avery", Username: "avery", FirstName: "Avery"},
			{ID: "jm", Username: "jm", FirstName: "James", LastName: "Merrill"},
		}, nil
	}
	got := c.chatMembers(context.Background(), &mattermost.Channel{ID: "c1", Type: "O"})
	if fmt.Sprint(asked) != "[avery jm]" {
		t.Fatalf("profiles requested for %v, want the other members in one request", asked)
	}
	for uid, want := range map[string]string{"avery": "Avery", "jm": "James Merrill"} {
		info := got.MemberMap[networkid.UserID(uid)].UserInfo
		if info == nil || info.Name == nil || *info.Name != want {
			t.Fatalf("%s UserInfo = %+v, want name %q", uid, info, want)
		}
		if info.Avatar == nil {
			t.Fatalf("%s UserInfo has no avatar; it would stop the ghost's avatar from loading", uid)
		}
	}
	if got.MemberMap["me"].UserInfo != nil {
		t.Fatal("own entry must not carry a ghost profile")
	}
}

func TestChatMembersProfileFailureStillListsMembers(t *testing.T) {
	c, _ := memberClient(map[string][]string{"c1": {"me", "avery"}}, false)
	c.usersByIDsFn = func([]string) ([]*mattermost.User, error) { return nil, errors.New("boom") }
	got := c.chatMembers(context.Background(), &mattermost.Channel{ID: "c1", Type: "O"})
	m, ok := got.MemberMap["avery"]
	if !ok || m.Membership != event.MembershipJoin {
		t.Fatalf("member dropped when profiles failed: %+v", got.MemberMap)
	}
	if m.UserInfo != nil {
		t.Fatalf("no profile was fetched, so no name should be pushed: %+v", m.UserInfo)
	}
	if !got.IsFull {
		t.Fatal("a profile failure must not change whether the member list is full")
	}
}

func TestUserUpdatedRefreshesGhost(t *testing.T) {
	p := newProcessEventsHarness(t, lifecycleOptions{probe: activeProbe})
	got := make(chan *mattermost.User, 1)
	p.client.refreshGhostFn = func(u *mattermost.User) { got <- u }
	p.deliver(&mattermost.UserUpdatedEvent{User: &mattermost.User{ID: "avery", FirstName: "Avery", LastName: "Wagar"}})
	select {
	case u := <-got:
		if u.ID != "avery" {
			t.Fatalf("refreshed %+v", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("user_updated did not refresh the ghost")
	}
	p.client.mu.RLock()
	defer p.client.mu.RUnlock()
	if u := p.client.users["avery"]; u == nil || u.LastName != "Wagar" {
		t.Fatal("user_updated did not update the cache")
	}
}

func TestUserInfoNameHasNoTrailingSpace(t *testing.T) {
	c := &MattermostClient{}
	info := c.userInfo(&mattermost.User{ID: "u", Username: "avery", FirstName: "Avery"})
	if *info.Name != "Avery" {
		t.Fatalf("name = %q", *info.Name)
	}
}

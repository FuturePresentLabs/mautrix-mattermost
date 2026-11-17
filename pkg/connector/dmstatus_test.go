package connector

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

const (
	dmSelfID  = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	dmOtherID = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var dmNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func customStatusProps(t *testing.T, cs mattermost.CustomStatus) map[string]any {
	t.Helper()
	raw, err := json.Marshal(cs)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"customStatus": string(raw)}
}

// dmClient has one DM with "alice", whose status and profile the test sets.
func dmClient(mode string) (*MattermostClient, *mattermost.Channel) {
	dm := &mattermost.Channel{ID: "dm1", Type: "D", Name: dmSelfID + "__" + dmOtherID}
	c := &MattermostClient{
		UserID:     dmSelfID,
		nameFormat: mattermost.NameFormatUsername,
		UserLogin:  &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login"}},
		channels:   map[string]*mattermost.Channel{dm.ID: dm},
		users:      map[string]*mattermost.User{dmOtherID: {ID: dmOtherID, Username: "alice"}},
		dmStatusFn: func() string { return mode },
		nowFn:      func() time.Time { return dmNow },
		channelMembersFn: func(string) ([]*mattermost.ChannelMember, error) {
			return nil, nil
		},
		usersByIDsFn: func([]string) ([]*mattermost.User, error) { return nil, nil },
	}
	c.presenceSeen = map[string]string{}
	return c, dm
}

func setStatus(c *MattermostClient, status string) {
	c.mu.Lock()
	c.presenceSeen[dmOtherID] = status
	c.mu.Unlock()
}

func TestDMTitles(t *testing.T) {
	beach := mattermost.CustomStatus{Emoji: "beach_umbrella", Text: "On vacation"}
	cases := []struct {
		name, mode, status string
		custom             *mattermost.CustomStatus
		title, topic       string
	}{
		{"online", "all", "online", nil, "alice 🟢", ""},
		{"away", "all", "away", nil, "alice 🟡", ""},
		{"do not disturb", "all", "dnd", nil, "alice ⛔", ""},
		{"offline", "all", "offline", nil, "alice ⚪", ""},
		{"not polled yet", "all", "", nil, "alice", ""},
		{"unknown status", "all", "out_of_office", nil, "alice", ""},
		{"empty mode means all", "", "away", nil, "alice 🟡", ""},
		{"custom status", "all", "online", &beach, "alice 🟢 ⛱", "On vacation"},
		{"server custom emoji left out", "all", "online", &mattermost.CustomStatus{Emoji: "party_parrot", Text: "Party"}, "alice 🟢", "Party"},
		{"text without emoji shows the speech balloon", "all", "online", &mattermost.CustomStatus{Text: "Busy"}, "alice 🟢 💬", "Busy"},
		{"dnd mode, online", "dnd", "online", nil, "alice", ""},
		{"dnd mode, away", "dnd", "away", nil, "alice", ""},
		{"dnd mode, dnd", "dnd", "dnd", nil, "alice ⛔", ""},
		{"dnd mode keeps custom status", "dnd", "online", &beach, "alice ⛱", "On vacation"},
		{"off", "off", "dnd", &beach, "alice", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, dm := dmClient(tc.mode)
			setStatus(c, tc.status)
			if tc.custom != nil {
				c.users[dmOtherID].Props = customStatusProps(t, *tc.custom)
			}
			title, topic := c.dmRoomInfo(dm)
			if title == nil || *title != tc.title {
				t.Fatalf("title = %v, want %q", deref(title), tc.title)
			}
			if topic == nil || *topic != tc.topic {
				t.Fatalf("topic = %v, want %q", deref(topic), tc.topic)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestDMTitleFollowsDisplayNames(t *testing.T) {
	c, dm := dmClient("all")
	c.users[dmOtherID].FirstName, c.users[dmOtherID].LastName = "Alice", "Example"
	c.nameFormat = mattermost.NameFormatFullName
	setStatus(c, "online")
	if title, _ := c.dmRoomInfo(dm); *title != "Alice Example 🟢" {
		t.Fatalf("title = %q", *title)
	}
}

func TestDMTitleUnknownPersonLeavesRoomAlone(t *testing.T) {
	c, dm := dmClient("all")
	delete(c.users, dmOtherID)
	if title, topic := c.dmRoomInfo(dm); title != nil || topic != nil {
		t.Fatalf("got %v / %v; an unknown profile must not blank the title", deref(title), deref(topic))
	}
	group := &mattermost.Channel{ID: "g", Type: "G", Name: "group"}
	if title, _ := c.dmRoomInfo(group); title != nil {
		t.Fatal("group DMs must keep their own title")
	}
}

func TestCustomStatusExpiry(t *testing.T) {
	cases := []struct {
		name string
		cs   mattermost.CustomStatus
		want bool
	}{
		{"don't clear never expires", mattermost.CustomStatus{Emoji: "palm_tree", ExpiresAt: dmNow.Add(-time.Hour)}, true},
		{"still running", mattermost.CustomStatus{Emoji: "palm_tree", Duration: "today", ExpiresAt: dmNow.Add(time.Hour)}, true},
		{"expired", mattermost.CustomStatus{Emoji: "palm_tree", Duration: "one_hour", ExpiresAt: dmNow.Add(-time.Minute)}, false},
		{"expires exactly now", mattermost.CustomStatus{Emoji: "palm_tree", Duration: "one_hour", ExpiresAt: dmNow}, false},
		{"empty", mattermost.CustomStatus{}, false},
	}
	for _, tc := range cases {
		u := &mattermost.User{Props: customStatusProps(t, tc.cs)}
		if got := u.ActiveCustomStatus(dmNow) != nil; got != tc.want {
			t.Errorf("%s: active = %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, props := range []map[string]any{nil, {"customStatus": "not json"}, {"customStatus": 42}} {
		if (&mattermost.User{Props: props}).ActiveCustomStatus(dmNow) != nil {
			t.Errorf("props %v produced a status", props)
		}
	}
}

func TestUserWithNonStringPropsDecodes(t *testing.T) {
	var u mattermost.User
	err := json.Unmarshal([]byte(`{"id":"u","username":"alice","props":{"customStatus":"{\"emoji\":\"palm_tree\"}","n":1,"b":true}}`), &u)
	if err != nil || u.ActiveCustomStatus(dmNow) == nil {
		t.Fatalf("decode: %v, status %+v", err, u.ActiveCustomStatus(dmNow))
	}
}

func TestDMStatusEventsOnlyOnChange(t *testing.T) {
	c, _ := dmClient("all")
	setStatus(c, "online")
	evs := c.dmStatusEvents()
	if len(evs) != 1 {
		t.Fatalf("first refresh queued %d events, want 1", len(evs))
	}
	info := evs[0].ChatInfoChange.ChatInfo
	if *info.Name != "alice 🟢" || !info.ExcludeChangesFromTimeline || evs[0].PortalKey.ID != "dm1" || evs[0].PortalKey.Receiver != "login" {
		t.Fatalf("event = %+v / %+v", evs[0].PortalKey, info)
	}
	if n := len(c.dmStatusEvents()); n != 0 {
		t.Fatalf("unchanged status queued %d events", n)
	}
	setStatus(c, "dnd")
	if evs := c.dmStatusEvents(); len(evs) != 1 || *evs[0].ChatInfoChange.ChatInfo.Name != "alice ⛔" {
		t.Fatalf("status change not queued: %+v", evs)
	}
}

func TestDMStatusExpiryClearsWithoutStatusChange(t *testing.T) {
	c, _ := dmClient("all")
	setStatus(c, "online")
	c.users[dmOtherID].Props = customStatusProps(t, mattermost.CustomStatus{Emoji: "palm_tree", Text: "Out", Duration: "one_hour", ExpiresAt: dmNow.Add(time.Minute)})
	if evs := c.dmStatusEvents(); len(evs) != 1 || *evs[0].ChatInfoChange.ChatInfo.Topic != "Out" {
		t.Fatalf("setup: %+v", evs)
	}
	c.nowFn = func() time.Time { return dmNow.Add(2 * time.Minute) }
	evs := c.dmStatusEvents()
	if len(evs) != 1 || *evs[0].ChatInfoChange.ChatInfo.Name != "alice 🟢" || *evs[0].ChatInfoChange.ChatInfo.Topic != "" {
		t.Fatalf("expired custom status not cleared: %+v", evs)
	}
}

func TestDMStatusSkipsArchivedAndNonDMChannels(t *testing.T) {
	c, dm := dmClient("all")
	setStatus(c, "online")
	dm.DeleteAt = 1
	c.channels["c"] = &mattermost.Channel{ID: "c", Type: "O", Name: "town-square"}
	if n := len(c.dmStatusEvents()); n != 0 {
		t.Fatalf("queued %d events for archived/non-DM channels", n)
	}
}

func TestRefreshDMStatusesOffQueuesNothing(t *testing.T) {
	c, _ := dmClient("off")
	setStatus(c, "online")
	var queued []bridgev2.RemoteEvent
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		queued = append(queued, ev)
		return bridgev2.EventHandlingResultQueued
	}
	c.refreshDMStatuses()
	if len(queued) != 0 {
		t.Fatalf("off mode queued %d events", len(queued))
	}
	c.dmStatusFn = func() string { return "all" }
	c.refreshDMStatuses()
	if len(queued) != 1 {
		t.Fatalf("all mode queued %d events, want 1", len(queued))
	}
}

// Room syncs title DMs with the same function, and record it so the next poll
// does not re-send the same title.
func TestResyncTitlesDMWithStatus(t *testing.T) {
	c, dm := dmClient("all")
	setStatus(c, "away")
	rs := c.channelResyncEvent(dm)
	info, err := rs.GetChatInfo(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if deref(info.Name) != "alice 🟡" || deref(info.Topic) != "" || !info.ExcludeChangesFromTimeline {
		t.Fatalf("resync info = %v / %v / %v", deref(info.Name), deref(info.Topic), info.ExcludeChangesFromTimeline)
	}
	if n := len(c.dmStatusEvents()); n != 0 {
		t.Fatalf("poll re-sent the title the resync just set (%d events)", n)
	}
	chInfo, err := c.GetChatInfo(context.Background(), &bridgev2.Portal{Portal: &database.Portal{PortalKey: rs.PortalKey}})
	if err == nil && chInfo != nil && deref(chInfo.Name) != "alice 🟡" {
		t.Fatalf("GetChatInfo title = %q", deref(chInfo.Name))
	}
}

func TestUserUpdatedRefreshesDMTitle(t *testing.T) {
	c, _ := dmClient("all")
	setStatus(c, "online")
	c.refreshGhostFn = func(*mattermost.User) {}
	got := make(chan string, 4)
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		if ch, ok := ev.(*simplevent.ChatInfoChange); ok && ch.ChatInfoChange.ChatInfo != nil && ch.ChatInfoChange.ChatInfo.Name != nil {
			got <- *ch.ChatInfoChange.ChatInfo.Name
		}
		return bridgev2.EventHandlingResultQueued
	}
	c.refreshDMStatuses()
	<-got
	updated := &mattermost.User{ID: dmOtherID, Username: "alice", Props: customStatusProps(t, mattermost.CustomStatus{Emoji: "palm_tree"})}
	c.handleUserUpdated(updated)
	select {
	case title := <-got:
		if title != "alice 🟢 🌴" {
			t.Fatalf("title after custom status = %q", title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("custom status change did not update the DM title")
	}
}

func TestInvalidDMStatusFailsStart(t *testing.T) {
	for _, ok := range []string{"", "all", "dnd", "off"} {
		if err := (&MattermostConnector{Config: &Config{DMStatus: ok}}).Start(nil); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	err := (&MattermostConnector{Config: &Config{DMStatus: "on"}}).Start(nil)
	if err == nil || !strings.Contains(err.Error(), "dm_status") {
		t.Fatalf("typo accepted or unclear error: %v", err)
	}
}

func TestStatusPollingRunsForDMTitlesWithoutPresence(t *testing.T) {
	for mode, wantTitles := range map[string]bool{"": true, "all": true, "dnd": true, "off": false} {
		c := &MattermostClient{UserLogin: &bridgev2.UserLogin{
			UserLogin: &database.UserLogin{ID: "login"},
			Bridge:    &bridgev2.Bridge{Network: &MattermostConnector{Config: &Config{DMStatus: mode}}},
		}}
		publish, titles := c.statusConsumers()
		if publish || titles != wantTitles {
			t.Errorf("dm_status %q: publish=%v titles=%v, want false/%v", mode, publish, titles, wantTitles)
		}
	}
}

func TestPolledStatusesUpdateDMTitles(t *testing.T) {
	c, _ := dmClient("all")
	var titles []string
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		if ch, ok := ev.(*simplevent.ChatInfoChange); ok {
			titles = append(titles, *ch.ChatInfoChange.ChatInfo.Name)
		}
		return bridgev2.EventHandlingResultQueued
	}
	c.applyPolledStatuses(map[string]string{dmOtherID: "away"}, false, true)
	c.applyPolledStatuses(map[string]string{dmOtherID: "away"}, false, true)
	c.applyPolledStatuses(map[string]string{dmOtherID: "dnd"}, false, true)
	if strings.Join(titles, "|") != "alice 🟡|alice ⛔" {
		t.Fatalf("titles = %q", titles)
	}
	c.applyPolledStatuses(map[string]string{dmOtherID: "online"}, false, false)
	if len(titles) != 2 {
		t.Fatal("titles updated with DM status off")
	}
}

func TestBotDMHasNoStatus(t *testing.T) {
	c, dm := dmClient("all")
	c.users[dmOtherID].IsBot = true
	setStatus(c, "offline")
	if title, _ := c.dmRoomInfo(dm); *title != "alice" {
		t.Fatalf("bot title = %q, want no status marker", *title)
	}
	var u mattermost.User
	if err := json.Unmarshal([]byte(`{"id":"b","username":"github","is_bot":true}`), &u); err != nil || !u.IsBot {
		t.Fatalf("is_bot not decoded: %v %+v", err, u)
	}
}

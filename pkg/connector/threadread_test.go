package connector

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// threadReadClient records the thread receipts it queues as
// "channel/root@HH:MM:SS" (channel receipts as "channel@...").
func threadReadClient(t *testing.T, collapsed bool) (*MattermostClient, *[]string) {
	t.Helper()
	c, _, _ := readClient()
	c.collapsedThreadsFn = func() bool { return collapsed }
	c.threadRootsFn = func(channelID string, since time.Time) []string {
		if !since.Equal(dmNow.Add(-threadLookback)) && since.Year() < 2000 {
			t.Errorf("bad lookback %v", since)
		}
		if channelID == "chan" {
			return []string{"r1", "r2"}
		}
		return []string{"z"} // other rooms: must never be asked for
	}
	c.unreadThreadsFn = func(team string) ([]string, error) {
		if team != "teamB" {
			t.Errorf("unread threads asked for team %q", team)
		}
		return []string{"r2"}, nil // Mattermost counts r2 as unread
	}
	var got []string
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		r := ev.(*simplevent.Receipt)
		if !r.Sender.IsFromMe || r.PortalKey.Receiver != "login" || r.GetType() != bridgev2.RemoteEventReadReceipt {
			t.Fatalf("bad receipt %+v", r)
		}
		key := string(r.PortalKey.ID)
		if r.ThreadRoot != "" {
			key += "/" + string(r.ThreadRoot)
			if r.GetReceiptThread() != r.ThreadRoot {
				t.Fatal("thread not exposed to the framework")
			}
		}
		got = append(got, key+"@"+r.ReadUpTo.UTC().Format("15:04:05"))
		return bridgev2.EventHandlingResultQueued
	}
	return c, &got
}

func at(ms int64) string { return time.UnixMilli(ms).UTC().Format("15:04:05") }

func TestThreadReadInMattermost(t *testing.T) {
	c, got := threadReadClient(t, true)
	ts := int64(1794950000000)
	c.handleThreadRead(&mattermost.ThreadReadEvent{ThreadID: "r1", ChannelID: "chan", Timestamp: ts})
	c.handleThreadRead(&mattermost.ThreadReadEvent{ThreadID: "r1", ChannelID: "chan", Timestamp: ts})        // repeat
	c.handleThreadRead(&mattermost.ThreadReadEvent{ThreadID: "r1", ChannelID: "chan", Timestamp: ts - 5000}) // older
	c.handleThreadRead(&mattermost.ThreadReadEvent{ThreadID: "r9", ChannelID: "elsewhere", Timestamp: ts})   // not bridged
	c.handleThreadRead(&mattermost.ThreadReadEvent{ThreadID: "r2", ChannelID: "chan"})                       // no time: now
	c.handleThreadRead(nil)
	if strings.Join(*got, ",") != "chan/r1@"+at(ts)+",chan/r2@"+dmNow.UTC().Format("15:04:05") {
		t.Fatalf("got %v", *got)
	}
}

func TestMatrixThreadReadNotEchoed(t *testing.T) {
	c, got := threadReadClient(t, true)
	ts := time.UnixMilli(1794950000000)
	if err := c.HandleMatrixReadReceipt(nil, receipt("chan", ts, "$root", false)); err != nil {
		t.Fatal(err)
	}
	c.handleThreadRead(&mattermost.ThreadReadEvent{ThreadID: "rootpost", ChannelID: "chan", Timestamp: ts.UnixMilli()})
	if len(*got) != 0 {
		t.Fatalf("echoed: %v", *got)
	}
}

func TestChannelViewReadsThreadsWhenInline(t *testing.T) {
	ts := int64(1794950000000)
	c, got := threadReadClient(t, false)
	c.handleChannelsViewed(&mattermost.ChannelsViewedEvent{Times: map[string]int64{"chan": ts}})
	if strings.Join(*got, ",") != "chan@"+at(ts)+",chan/r1@"+at(ts)+",chan/r2@"+at(ts) {
		t.Fatalf("inline: %v", *got)
	}
	// Collapsed: threads Mattermost counts as unread stay unread.
	c2, got2 := threadReadClient(t, true)
	c2.handleChannelsViewed(&mattermost.ChannelsViewedEvent{Times: map[string]int64{"chan": ts}})
	if strings.Join(*got2, ",") != "chan@"+at(ts)+",chan/r1@"+at(ts) {
		t.Fatalf("collapsed: %v", *got2)
	}
	// Unread count unavailable: no thread is marked.
	c3, got3 := threadReadClient(t, true)
	c3.unreadThreadsFn = func(string) ([]string, error) { return nil, errors.New("down") }
	c3.handleChannelsViewed(&mattermost.ChannelsViewedEvent{Times: map[string]int64{"chan": ts}})
	if strings.Join(*got3, ",") != "chan@"+at(ts) {
		t.Fatalf("unread unknown: %v", *got3)
	}
	// A channel without a team (DM): nothing to ask, nothing marked.
	if _, ok := c3.unreadThreads(""); ok {
		t.Fatal("unread threads for no team")
	}
}

func TestThreadCatchUp(t *testing.T) {
	ts := int64(1794950000000)
	mk := func(id, ch string, viewed, unread int64) *mattermost.UserThread {
		th := &mattermost.UserThread{ID: id, LastViewedAt: viewed, UnreadReplies: unread}
		th.Post.ChannelID = ch
		return th
	}
	for _, collapsed := range []bool{true, false} {
		c, got := threadReadClient(t, collapsed)
		var teams []string
		c.userThreadsFn = func(teamID string) ([]*mattermost.UserThread, error) {
			teams = append(teams, teamID)
			if teamID == "bad" {
				return nil, errors.New("down")
			}
			return []*mattermost.UserThread{
				mk("read", "chan", ts, 0),
				mk("unread", "chan", ts, 2),
				mk("never", "chan", 0, 0),
				mk("foreign", "elsewhere", ts, 0),
				nil,
			}, nil
		}
		c.noteMemberships([]*mattermost.ChannelMember{{ChannelID: "chan", LastViewedAt: ts + 1000}, {ChannelID: "x"}, {ChannelID: "elsewhere", LastViewedAt: ts}})
		c.catchUpThreadReads([]*mattermost.Team{{ID: "bad"}, {ID: "teamB"}})
		sort.Strings(*got)
		want := []string{"chan/read@" + at(ts), "chan@" + at(ts+1000), "chan/r1@" + at(ts+1000)}
		if !collapsed {
			want = append(want, "chan/r2@"+at(ts+1000))
		}
		sort.Strings(want)
		if strings.Join(*got, ",") != strings.Join(want, ",") || strings.Join(teams, ",") != "bad,teamB" {
			t.Fatalf("collapsed=%v: got %v teams %v", collapsed, *got, teams)
		}
	}
}

func TestCollapsedThreadsCachedAndReset(t *testing.T) {
	c := &MattermostClient{Token: "tok", baseURL: "http://127.0.0.1:1"}
	if !c.collapsedThreads() {
		t.Fatal("unreachable server: threads must count as collapsed")
	}
	c.collapsedOn, c.collapsedKnown = false, true
	if c.collapsedThreads() {
		t.Fatal("cached value ignored")
	}
	c.resetCollapsedThreads()
	if !c.collapsedThreads() {
		t.Fatal("not re-read after reset")
	}
}

func TestThreadReceiptGuards(t *testing.T) {
	c, got := threadReadClient(t, true)
	c.queueThreadReceipt("chan", "", dmNow)
	c.queueThreadReceipt("", "r1", dmNow)
	c.queueThreadReceipt("chan", "r1", time.Time{})
	if len(*got) != 0 {
		t.Fatalf("sent %v", *got)
	}
}

func TestCatchUpRereadsCollapsedSetting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"CollapsedThreads":"disabled"}`))
	}))
	defer srv.Close()
	c, _ := threadReadClient(t, true)
	c.collapsedThreadsFn = nil
	c.baseURL, c.Token = srv.URL, "tok"
	c.collapsedOn, c.collapsedKnown = true, true // stale from the last connect
	c.userThreadsFn = func(string) ([]*mattermost.UserThread, error) { return nil, nil }
	c.catchUpThreadReads(nil)
	if c.collapsedThreads() {
		t.Fatal("setting not re-read on connect")
	}
}

func TestThreadReadEventDispatched(t *testing.T) {
	c, got := threadReadClient(t, true)
	feed := make(chan any, 1)
	c.client = &mattermost.Client{Events: feed}
	feed <- &mattermost.ThreadReadEvent{ThreadID: "r1", ChannelID: "chan", Timestamp: 1794950000000}
	close(feed)
	c.processEvents()
	if strings.Join(*got, ",") != "chan/r1@"+at(1794950000000) {
		t.Fatalf("got %v", *got)
	}
}

func TestSyncCatchesUpOwnTeamsOnly(t *testing.T) {
	c, _ := accountClient(t, map[string]bool{"tA": true}, nil)
	var teams []string
	c.userThreadsFn = func(team string) ([]*mattermost.UserThread, error) { teams = append(teams, team); return nil, nil }
	c.collapsedThreadsFn = func() bool { return true }
	c.syncChannels()
	if strings.Join(teams, ",") != "tA" {
		t.Fatalf("caught up teams %v", teams)
	}
}

func TestReceiptsStampedWhenSent(t *testing.T) {
	c, _ := threadReadClient(t, false)
	var stamps []time.Time
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		r := ev.(*simplevent.Receipt)
		if r.ReadUpTo.After(dmNow) {
			t.Fatalf("read up to moved: %v", r.ReadUpTo)
		}
		stamps = append(stamps, r.Timestamp)
		return bridgev2.EventHandlingResultQueued
	}
	old := dmNow.Add(-5 * time.Hour)
	c.handleChannelsViewed(&mattermost.ChannelsViewedEvent{Times: map[string]int64{"chan": old.UnixMilli()}})
	if len(stamps) != 3 {
		t.Fatalf("receipts %d", len(stamps))
	}
	for _, s := range stamps {
		if !s.Equal(dmNow) {
			t.Fatalf("stamped %v, want now %v", s, dmNow)
		}
	}
	if got := c.receiptTime(dmNow.Add(time.Minute)); !got.Equal(dmNow.Add(time.Minute)) {
		t.Fatalf("future read stamped %v", got)
	}
}

func TestRoomReceiptTargetsMainTimeline(t *testing.T) {
	c, _ := threadReadClient(t, true)
	c.unreadThreadsFn = func(string) ([]string, error) { return nil, nil }
	var asked time.Time
	c.lastMainMessageFn = func(ch string, at time.Time) networkid.MessageID { asked = at; return "main1" }
	var targets []string
	c.queueRemoteEventFn = func(ev bridgev2.RemoteEvent) bridgev2.EventHandlingResult {
		r := ev.(*simplevent.Receipt)
		targets = append(targets, string(r.ThreadRoot)+">"+string(r.LastTarget))
		return bridgev2.EventHandlingResultQueued
	}
	ts := int64(1794950000000)
	c.handleChannelsViewed(&mattermost.ChannelsViewedEvent{Times: map[string]int64{"chan": ts}})
	if len(targets) == 0 || targets[0] != ">main1" || !asked.Equal(time.UnixMilli(ts)) {
		t.Fatalf("targets %v asked %v", targets, asked)
	}
	for _, tg := range targets[1:] {
		if strings.HasSuffix(tg, ">main1") {
			t.Fatalf("thread receipt given the room target: %v", targets)
		}
	}
}

package mattermost

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChannelMemberMuted(t *testing.T) {
	cases := map[string]bool{"mention": true, "all": false, "": false}
	for v, want := range cases {
		m := &ChannelMember{NotifyProps: map[string]string{"mark_unread": v}}
		if m.Muted() != want {
			t.Errorf("mark_unread %q: muted = %v", v, m.Muted())
		}
	}
	var nilMember *ChannelMember
	if nilMember.Muted() {
		t.Error("nil member muted")
	}
}

func TestMembershipRequests(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`[{"channel_id":"c1","user_id":"me","notify_props":{"mark_unread":"mention","push":"default"}}]`))
	}))
	defer srv.Close()
	members, err := GetMyChannelMembers(srv.URL, "tok", "t1")
	if err != nil || gotPath != "/api/v4/users/me/teams/t1/channels/members" || len(members) != 1 || !members[0].Muted() {
		t.Fatalf("members %v err %v path %s", members, err, gotPath)
	}
}

func TestChannelMemberUpdatedEvent(t *testing.T) {
	c := &Client{Events: make(chan any, 2)}
	c.parseEvent([]byte(`{"event":"channel_member_updated","data":{"channelMember":"{\"channel_id\":\"c1\",\"user_id\":\"me\",\"notify_props\":{\"mark_unread\":\"mention\"}}"}}`))
	c.parseEvent([]byte(`{"event":"channel_member_updated","data":{"channelMember":"not json"}}`))
	c.parseEvent([]byte(`{"event":"channel_member_updated","data":{"channelMember":"{\"user_id\":\"me\"}"}}`))
	var got []any
	for {
		select {
		case ev := <-c.Events:
			got = append(got, ev)
			continue
		default:
		}
		break
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1 (bad and channel-less members dropped)", len(got))
	}
	ev, ok := got[0].(*ChannelMemberUpdatedEvent)
	if !ok || ev.Member.ChannelID != "c1" || !ev.Member.Muted() {
		t.Fatalf("event = %#v", got[0])
	}
}

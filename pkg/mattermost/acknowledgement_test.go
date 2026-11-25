package mattermost

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAcknowledgement(t *testing.T) {
	const valid = `{"user_id":"alice","post_id":"post","acknowledged_at":1700000000000,"channel_id":"channel","remote_id":"remote"}`
	for _, tc := range []struct {
		name, action, channel string
		payload               any
		want                  bool
	}{
		{"added", "post_acknowledgement_added", "channel", valid, true},
		{"removed", "post_acknowledgement_removed", "channel", valid, true},
		{"broadcast fallback", "post_acknowledgement_added", "channel", `{"user_id":"alice","post_id":"post","acknowledged_at":1}`, true},
		{"mismatch", "post_acknowledgement_added", "other", valid, false},
		{"malformed", "post_acknowledgement_added", "channel", "{", false},
		{"null", "post_acknowledgement_added", "channel", "null", false},
		{"object", "post_acknowledgement_added", "channel", map[string]any{"post_id": "post"}, false},
		{"zero", "post_acknowledgement_added", "channel", `{"user_id":"alice","post_id":"post","acknowledged_at":0}`, false},
		{"blank actor", "post_acknowledgement_removed", "channel", `{"user_id":" ","post_id":"post","acknowledged_at":1}`, false},
		{"blank post", "post_acknowledgement_added", "channel", `{"user_id":"alice","post_id":"","acknowledged_at":1}`, false},
		{"missing channel", "post_acknowledgement_added", "", `{"user_id":"alice","post_id":"post","acknowledged_at":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient("", "")
			frame, err := json.Marshal(WSEvent{Event: tc.action, Data: map[string]any{"acknowledgement": tc.payload}, Broadcast: WSBroadcast{ChannelID: tc.channel}})
			if err != nil {
				t.Fatal(err)
			}
			c.parseEvent(frame)
			select {
			case got := <-c.Events:
				if !tc.want {
					t.Fatalf("unexpected event %#v", got)
				}
				ack, ok := got.(*AcknowledgementEvent)
				if !ok || ack.IsAdd != (tc.action == "post_acknowledgement_added") ||
					ack.PostID != "post" || ack.UserID != "alice" || ack.ChannelID != tc.channel ||
					ack.AcknowledgedAt <= 0 {
					t.Fatalf("incorrect decoded acknowledgement: %#v", got)
				}
			default:
				if tc.want {
					t.Fatal("acknowledgement event was ignored")
				}
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestPostPriorityDecodesRequestedAck(t *testing.T) {
	var p Post
	raw := `{"id":"p","metadata":{"priority":{"priority":"important","requested_ack":true}}}`
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if !p.RequestsAcknowledgement() {
		t.Fatalf("requested_ack was not decoded: %+v", p.Metadata)
	}
}

func TestPostAcknowledgementPredicates(t *testing.T) {
	asks := &Post{Metadata: &PostMetadata{Priority: &PostPriority{RequestedAck: boolPtr(true)}}}
	declined := &Post{Metadata: &PostMetadata{Priority: &PostPriority{RequestedAck: boolPtr(false)}}}
	noFlag := &Post{Metadata: &PostMetadata{Priority: &PostPriority{}}}
	for name, tc := range map[string]struct {
		post *Post
		want bool
	}{
		"requested":         {asks, true},
		"explicitly false":  {declined, false},
		"priority, no flag": {noFlag, false},
		"no priority":       {&Post{Metadata: &PostMetadata{}}, false},
		"no metadata":       {&Post{}, false},
		"nil post":          {nil, false},
	} {
		if got := tc.post.RequestsAcknowledgement(); got != tc.want {
			t.Errorf("%s: RequestsAcknowledgement() = %t, want %t", name, got, tc.want)
		}
	}

	acked := &Post{Metadata: &PostMetadata{Acknowledgements: []*Acknowledgement{
		nil,
		{UserID: "bob", AcknowledgedAt: 5},
		{UserID: "me", AcknowledgedAt: 0}, // withdrawn: the server keeps the row with a zero timestamp
		{UserID: "alice", AcknowledgedAt: 7},
	}}}
	if !acked.AcknowledgedBy("alice") || acked.AcknowledgedBy("me") || acked.AcknowledgedBy("carol") || acked.AcknowledgedBy("") {
		t.Error("AcknowledgedBy must be true only for an active acknowledgement by that user")
	}

	reacted := &Post{Metadata: &PostMetadata{Reactions: []*Reaction{
		{UserID: "me", EmojiName: "thumbsup"},
		{UserID: "me", EmojiName: "joy", DeleteAt: 9},
		{UserID: "bob", EmojiName: "heart"},
	}}}
	if !reacted.ReactedBy("me", "thumbsup") || reacted.ReactedBy("me", "joy") || reacted.ReactedBy("me", "heart") || reacted.ReactedBy("bob", "thumbsup") || reacted.ReactedBy("me", "") {
		t.Error("ReactedBy must match user and emoji together and ignore deleted reactions")
	}
}

func TestAcknowledgePostCallsTheServerAndReportsRefusals(t *testing.T) {
	var method, path, auth string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		w.WriteHeader(status)
		if status >= 400 {
			_, _ = w.Write([]byte(`{"id":"api.refused","message":"refused","status_code":403}`))
		}
	}))
	defer srv.Close()
	if err := AcknowledgePost(srv.URL, "tok", "u1", "p1"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/v4/users/u1/posts/p1/ack" || auth != "Bearer tok" {
		t.Fatalf("request = %s %s auth=%q", method, path, auth)
	}
	status = http.StatusForbidden
	if err := AcknowledgePost(srv.URL, "tok", "u1", "p1"); err == nil {
		t.Fatal("a refused acknowledgement must be an error")
	}
}

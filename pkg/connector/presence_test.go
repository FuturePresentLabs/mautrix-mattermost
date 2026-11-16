package connector

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// recordSetter captures what would have been sent to the homeserver.
type recordSetter struct {
	calls []mautrix.ReqPresence
	mxids []id.UserID
	err   error
}

func (r *recordSetter) set(_ context.Context, mxid id.UserID, req mautrix.ReqPresence) error {
	r.calls = append(r.calls, req)
	r.mxids = append(r.mxids, mxid)
	return r.err
}

// TestMatrixPresence pins the Mattermost → Matrix status mapping. Mattermost's Do
// Not Disturb has no Matrix equivalent, so it must ride on the status message of an
// online presence rather than being dropped or silently mapped to away.
func TestMatrixPresence(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		wantOK    bool
		presence  string
		statusMsg string
	}{
		{name: "online", status: "online", wantOK: true, presence: "online"},
		{name: "away", status: "away", wantOK: true, presence: "unavailable"},
		{name: "dnd", status: "dnd", wantOK: true, presence: "online", statusMsg: "Do Not Disturb"},
		{name: "offline", status: "offline", wantOK: true, presence: "offline"},
		{name: "empty", status: "", wantOK: false},
		{name: "unknown", status: "on-vacation", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matrixPresence(tc.status)
			if ok != tc.wantOK {
				t.Fatalf("matrixPresence(%q) ok = %v, want %v", tc.status, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if string(got.Presence) != tc.presence {
				t.Errorf("presence = %q, want %q", got.Presence, tc.presence)
			}
			if got.StatusMsg != tc.statusMsg {
				t.Errorf("status message = %q, want %q", got.StatusMsg, tc.statusMsg)
			}
		})
	}
}

// TestPublishPresenceSendsGhostMXID checks that the ghost's MXID — not the
// Mattermost user ID — is what reaches the homeserver.
func TestPublishPresenceSendsGhostMXID(t *testing.T) {
	setter := &recordSetter{}
	mxid := id.UserID("@mm_abc:beeper.local")
	resolve := func(context.Context, string) (id.UserID, bool, error) { return mxid, true, nil }

	err := publishPresence(context.Background(), setter.set, resolve, presenceUpdate{userID: "abc", status: "away"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(setter.calls) != 1 {
		t.Fatalf("published %d presences, want 1", len(setter.calls))
	}
	if setter.mxids[0] != mxid {
		t.Errorf("published for %q, want %q", setter.mxids[0], mxid)
	}
	if setter.calls[0].Presence != "unavailable" {
		t.Errorf("presence = %q, want unavailable", setter.calls[0].Presence)
	}
}

// TestPublishPresenceSkipsUserWithoutGhost covers the rule that status changes must
// not create ghosts: a user the bridge never bridged is skipped, not registered.
func TestPublishPresenceSkipsUserWithoutGhost(t *testing.T) {
	setter := &recordSetter{}
	resolve := func(context.Context, string) (id.UserID, bool, error) { return "", false, nil }

	if err := publishPresence(context.Background(), setter.set, resolve, presenceUpdate{userID: "abc", status: "online"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(setter.calls) != 0 {
		t.Fatalf("published %d presences for a user with no ghost, want 0", len(setter.calls))
	}
}

// TestPublishPresenceSkipsUnmappedStatus covers a status the mapping does not know:
// the ghost must not be resolved at all, since there is nothing to publish.
func TestPublishPresenceSkipsUnmappedStatus(t *testing.T) {
	setter := &recordSetter{}
	resolved := false
	resolve := func(context.Context, string) (id.UserID, bool, error) {
		resolved = true
		return "@mm_abc:beeper.local", true, nil
	}

	if err := publishPresence(context.Background(), setter.set, resolve, presenceUpdate{userID: "abc", status: ""}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved {
		t.Error("resolved a ghost for a status that is not published")
	}
	if len(setter.calls) != 0 {
		t.Fatalf("published %d presences, want 0", len(setter.calls))
	}
}

// TestPublishPresenceReportsErrors makes sure a homeserver failure is visible to the
// caller (which logs it) instead of being swallowed.
func TestPublishPresenceReportsErrors(t *testing.T) {
	boom := errors.New("homeserver said no")
	t.Run("set fails", func(t *testing.T) {
		setter := &recordSetter{err: boom}
		resolve := func(context.Context, string) (id.UserID, bool, error) { return "@mm_abc:beeper.local", true, nil }
		if err := publishPresence(context.Background(), setter.set, resolve, presenceUpdate{userID: "abc", status: "online"}); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
	})
	t.Run("ghost lookup fails", func(t *testing.T) {
		setter := &recordSetter{}
		resolve := func(context.Context, string) (id.UserID, bool, error) { return "", false, boom }
		if err := publishPresence(context.Background(), setter.set, resolve, presenceUpdate{userID: "abc", status: "online"}); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want %v", err, boom)
		}
		if len(setter.calls) != 0 {
			t.Fatalf("published %d presences after a failed lookup, want 0", len(setter.calls))
		}
	})
}

// TestHandleStatusChangeIgnoresSelf checks that the logged-in user's own status does
// not reach the bridge: their MXID is a real Matrix account whose presence is owned by
// their own clients.
func TestHandleStatusChangeIgnoresSelf(t *testing.T) {
	c := &MattermostClient{UserID: "self", presenceUpdates: make(chan presenceUpdate, 4)}
	c.handleStatusChange(&mattermost.StatusChangeEvent{UserID: "self", Status: "online"})
	if len(c.presenceUpdates) != 0 {
		t.Fatalf("queued %d updates for the logged-in user, want 0", len(c.presenceUpdates))
	}
	c.handleStatusChange(&mattermost.StatusChangeEvent{UserID: "other", Status: "away"})
	if len(c.presenceUpdates) != 1 {
		t.Fatalf("queued %d updates for another user, want 1", len(c.presenceUpdates))
	}
	if got := <-c.presenceUpdates; got.userID != "other" || got.status != "away" {
		t.Errorf("queued %+v, want other/away", got)
	}
}

// TestQueuePresenceUpdateNeverBlocks covers the rule that status churn cannot stall
// the websocket loop: with the queue full, new updates are dropped.
func TestQueuePresenceUpdateNeverBlocks(t *testing.T) {
	c := &MattermostClient{presenceUpdates: make(chan presenceUpdate, 1)}
	for i := 0; i < 5; i++ {
		c.queuePresenceUpdate(presenceUpdate{userID: "abc", status: "online"})
	}
	if len(c.presenceUpdates) != 1 {
		t.Fatalf("queue holds %d updates, want 1", len(c.presenceUpdates))
	}
	// No publisher installed (presence disabled or before Connect): dropping is fine.
	empty := &MattermostClient{}
	empty.queuePresenceUpdate(presenceUpdate{userID: "abc", status: "online"})
}

// TestPresenceConfigDefaultsOff pins the opt-in default. Turning presence on must be a
// deliberate config change, because it adds a homeserver request per status change.
func TestPresenceConfigDefaultsOff(t *testing.T) {
	var empty Config
	if err := yaml.Unmarshal([]byte("{}"), &empty); err != nil {
		t.Fatalf("unmarshal empty config: %v", err)
	}
	if empty.Presence.Enabled {
		t.Fatal("presence is enabled without being configured")
	}

	var on Config
	if err := yaml.Unmarshal([]byte("presence:\n  enabled: true\n"), &on); err != nil {
		t.Fatalf("unmarshal presence config: %v", err)
	}
	if !on.Presence.Enabled {
		t.Fatal("presence.enabled: true was not read")
	}
}

// TestExampleConfigsDocumentPresence keeps the shipped templates and the embedded
// example in step: both must carry the presence block, turned off.
func TestExampleConfigsDocumentPresence(t *testing.T) {
	for name, body := range map[string]string{
		"ExampleConfig":       ExampleConfig,
		"example-config.yaml": mustReadExampleConfig(t),
	} {
		t.Run(name, func(t *testing.T) {
			var network struct {
				Network Config `yaml:"network"`
			}
			if err := yaml.Unmarshal([]byte(body), &network); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if network.Network.Presence.Enabled {
				t.Error("the example config ships with presence enabled")
			}
			if !strings.Contains(body, "presence:") {
				t.Error("the example config does not document the presence block")
			}
		})
	}
}

func mustReadExampleConfig(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../../example-config.yaml")
	if err != nil {
		t.Fatalf("read example-config.yaml: %v", err)
	}
	return string(body)
}

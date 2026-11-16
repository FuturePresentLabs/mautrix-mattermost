package connector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/util/configupgrade"
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

// TestPresenceDiffPublishesOnlyChanges is the rule that keeps a poll from turning into
// a flood: unchanged statuses are not republished, and the order is stable so a poll is
// reproducible in tests and logs.
func TestPresenceDiffPublishesOnlyChanges(t *testing.T) {
	previous := map[string]string{"a": "online", "b": "away"}
	next := map[string]string{"a": "online", "b": "dnd", "c": "offline"}

	updates := presenceDiff(previous, next)
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2: %+v", len(updates), updates)
	}
	if updates[0].userID != "b" || updates[0].status != "dnd" {
		t.Errorf("first update = %+v, want b/dnd", updates[0])
	}
	if updates[1].userID != "c" || updates[1].status != "offline" {
		t.Errorf("second update = %+v, want c/offline", updates[1])
	}
}

// TestPresenceDiffFirstPoll covers a cold start: nothing has been published yet, so
// every known status is a change.
func TestPresenceDiffFirstPoll(t *testing.T) {
	updates := presenceDiff(nil, map[string]string{"a": "online", "b": "offline"})
	if len(updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(updates))
	}
}

// TestPresenceDiffIgnoresMissingUsers pins the rule that a user vanishing from the
// status answer is silence, not evidence they went offline.
func TestPresenceDiffIgnoresMissingUsers(t *testing.T) {
	updates := presenceDiff(map[string]string{"a": "online"}, map[string]string{})
	if len(updates) != 0 {
		t.Fatalf("got %d updates for a missing user, want 0", len(updates))
	}
}

// TestPresenceCandidatesSkipsSelf checks that the logged-in user never enters the poll
// set: their MXID is a real Matrix account whose presence their own clients own.
func TestPresenceCandidatesSkipsSelf(t *testing.T) {
	c := &MattermostClient{
		UserID:       "self",
		presenceSeen: map[string]string{},
		users: map[string]*mattermost.User{
			"self":  {ID: "self", Username: "avery"},
			"other": {ID: "other", Username: "james"},
			"":      {},
		},
	}
	got := c.presenceCandidates()
	if len(got) != 1 || got[0] != "other" {
		t.Fatalf("candidates = %v, want [other]", got)
	}
}

// TestHandleStatusChangeWakesPollerOnce covers the hint path: a status_change frame
// may arrive in bursts, and the poller only needs one pending wake-up.
func TestHandleStatusChangeWakesPollerOnce(t *testing.T) {
	wake := make(chan struct{}, 1)
	c := &MattermostClient{presenceWake: wake}
	c.handleStatusChange(&mattermost.StatusChangeEvent{UserID: "other", Status: "away"})
	c.handleStatusChange(&mattermost.StatusChangeEvent{UserID: "other", Status: "dnd"})
	if len(wake) != 1 {
		t.Fatalf("poller woken %d times, want 1", len(wake))
	}
	// No poller installed (presence disabled): a hint must be dropped, not panic.
	(&MattermostClient{}).handleStatusChange(&mattermost.StatusChangeEvent{UserID: "other"})
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
// example in step: both must carry the presence block, turned off. The embedded one is
// the network section on its own — the framework indents it under `network:` when it
// writes a config, which is what the standalone template shows.
func TestExampleConfigsDocumentPresence(t *testing.T) {
	t.Run("ExampleConfig", func(t *testing.T) {
		var cfg Config
		if err := yaml.Unmarshal([]byte(ExampleConfig), &cfg); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if cfg.Presence.Enabled {
			t.Error("the embedded example config ships with presence enabled")
		}
	})
	t.Run("example-config.yaml", func(t *testing.T) {
		body := mustReadExampleConfig(t)
		var network struct {
			Network Config `yaml:"network"`
		}
		if err := yaml.Unmarshal([]byte(body), &network); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if network.Network.Presence.Enabled {
			t.Error("example-config.yaml ships with presence enabled")
		}
	})
}

func mustReadExampleConfig(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../../example-config.yaml")
	if err != nil {
		t.Fatalf("read example-config.yaml: %v", err)
	}
	return string(body)
}

// TestPresenceSurvivesConfigUpgrade reproduces the bridge framework's config path: the
// file goes through the upgrader that mxmain builds (full example + network proxy), then
// the network node is decoded into Config. A flag that cannot round-trip is a flag the
// bridge silently ignores at startup.
func TestPresenceSurvivesConfigUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("network:\n    presence:\n        enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	example, _, networkUpgrader := (&MattermostConnector{}).GetConfig()
	var base strings.Builder
	base.WriteString("network:\n")
	for _, line := range strings.Split(example, "\n") {
		base.WriteString("    " + line + "\n")
	}
	upgrader := configupgrade.MergeUpgraders(base.String(),
		&configupgrade.ProxyUpgrader{Target: networkUpgrader, Prefix: []string{"network"}})
	data, _, err := configupgrade.Do(path, false, upgrader)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	var full struct {
		Network yaml.Node `yaml:"network"`
	}
	if err := yaml.Unmarshal(data, &full); err != nil {
		t.Fatalf("parse upgraded config: %v", err)
	}
	var cfg Config
	if err := full.Network.Decode(&cfg); err != nil {
		t.Fatalf("decode network: %v", err)
	}
	if !cfg.Presence.Enabled {
		t.Fatalf("presence lost in upgrade; upgraded yaml:\n%s", data)
	}
}

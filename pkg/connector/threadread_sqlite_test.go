package connector

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"
)

// The thread-roots query runs against the framework's real schema.
func TestThreadRootsQuerySQLite(t *testing.T) {
	raw, err := dbutil.NewWithDialect(filepath.Join(t.TempDir(), "bridge.db"), "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	db := database.New("test", (&MattermostConnector{}).GetDBMetaTypes(), raw)
	if err = db.Upgrade(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	for _, key := range []networkid.PortalKey{{ID: "ch1", Receiver: "me"}, {ID: "ch1", Receiver: "other"}, {ID: "ch2", Receiver: "me"}} {
		if err = db.Portal.Insert(t.Context(), &database.Portal{PortalKey: key, MXID: "", Metadata: &PortalMetadata{}}); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	add := func(room, receiver, root string, at time.Time) {
		n++
		msg := &database.Message{ID: networkid.MessageID("m" + string(rune('a'+n))), MXID: id.EventID("$e" + string(rune('a'+n))),
			Room:     networkid.PortalKey{ID: networkid.PortalID(room), Receiver: networkid.UserLoginID(receiver)},
			SenderID: "u", Timestamp: at, ThreadRoot: networkid.MessageID(root)}
		if err := db.Message.Insert(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
	}
	add("ch1", "me", "rootA", now.Add(-time.Hour))
	add("ch1", "me", "rootA", now.Add(-time.Minute)) // same thread twice
	add("ch1", "me", "rootB", now.Add(-2*time.Hour))
	add("ch1", "me", "", now)                              // not a thread reply
	add("ch1", "me", "rootOld", now.Add(-40*24*time.Hour)) // too old
	add("ch1", "other", "rootOther", now)                  // another login
	add("ch2", "me", "rootC", now)                         // another room
	c := &MattermostClient{UserLogin: &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "me"}, Bridge: &bridgev2.Bridge{ID: "test", DB: db}}}
	got := c.threadRoots("ch1", now.Add(-threadLookback))
	sort.Strings(got)
	if strings.Join(got, ",") != "rootA,rootB" {
		t.Fatalf("roots %v", got)
	}
}

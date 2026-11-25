package connector

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// seedRevealedBurn writes the state the reveal path leaves behind: the body is
// the revealed text, so the row is marked unsafe and, distinctly, revealed by
// this account's own reveal. A removal time for the original event already
// exists because the reveal scheduled it before sending the text.
func seedRevealedBurn(t *testing.T, f *ackFixture, expire time.Time) {
	t.Helper()
	f.source.Metadata = &MessageMetadata{AckPromptTracked: true, AckPromptUnsafe: true, AckPromptRevealedBurn: true}
	mustAck(t, f.bridge.DB.Message.Update(t.Context(), f.source))
	insertDisappear(t, f, expire)
}

func insertDisappear(t *testing.T, f *ackFixture, at time.Time) {
	t.Helper()
	_, err := f.bridge.DB.Exec(t.Context(), `INSERT INTO disappearing_message
		(bridge_id, mx_room, mxid, timestamp, type, timer, disappear_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		f.bridge.ID, "!room:example.test", f.source.MXID, time.Now().UnixNano(), "after_send", int64(time.Minute), at.UnixNano())
	mustAck(t, err)
}

func disappearRows(t *testing.T, f *ackFixture) (int, int64) {
	t.Helper()
	var count int
	var at int64
	mustAck(t, f.bridge.DB.QueryRow(t.Context(),
		`SELECT COUNT(*), COALESCE(MAX(disappear_at), 0) FROM disappearing_message WHERE mxid=$1`,
		f.source.MXID).Scan(&count, &at))
	return count, at
}

func revealedBurnSnapshot(expire time.Time, message string, actors ...string) ackSnapshot {
	return ackSnapshot{asking: true, author: "alice", message: message, burn: true,
		actors: actors, revealExpireAt: expire.UnixMilli()}
}

// A revealed burn-on-read text is the one unsafe body with a rebuild path. Once
// this account acknowledges, the instruction must go without re-revealing the
// post, without holding its plaintext, and without changing its removal time.
// Reopening the SQLite database through an independent connection verifies
// durable flags, not just the bridge's cached message object. Replay and stale
// frames must still converge on the authoritative snapshot after reloading.
func TestAcknowledgementPromptRevealedBurnDurableStateAndReplay(t *testing.T) {
	f := newAckFixture(t, "")
	expire := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	seedRevealedBurn(t, f, expire)
	var seq int
	var name, dbPath string
	mustAck(t, f.bridge.DB.QueryRow(t.Context(), "PRAGMA database_list").Scan(&seq, &name, &dbPath))
	readDurable := func(wantHidden bool) {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
		mustAck(t, err)
		defer db.Close()
		var raw string
		mustAck(t, db.QueryRow("SELECT metadata FROM message WHERE rowid=?", f.source.RowID).Scan(&raw))
		var meta MessageMetadata
		mustAck(t, json.Unmarshal([]byte(raw), &meta))
		if meta.AckPromptHidden != wantHidden || !meta.AckPromptTracked || !meta.AckPromptRevealedBurn || !meta.AckPromptUnsafe {
			t.Fatalf("reopened metadata = %+v; hidden=%t and all tracking/safety flags must survive", meta, wantHidden)
		}
	}
	readDurable(false)
	f.snapshot.Store(revealedBurnSnapshot(expire, secretText, "self"))
	runAckFrames(t, f.client, ackFrameFor("self", true))
	readDurable(true)
	// Duplicate and stale remove hints cannot override the authoritative self ack.
	runAckFrames(t, f.client, ackFrameFor("self", true), ackFrameFor("self", false))
	if len(f.bot.edited) != 1 {
		t.Fatalf("replayed frames generated %d edits, want one", len(f.bot.edited))
	}
	readDurable(true)
	f.snapshot.Store(revealedBurnSnapshot(expire, secretText))
	runAckFrames(t, f.client, ackFrameFor("self", false))
	readDurable(false)
	// A stale add hint cannot re-hide the prompt once the server says withdrawn.
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 2 {
		t.Fatalf("stale add generated %d total edits, want two", len(f.bot.edited))
	}
	readDurable(false)
}

func TestAcknowledgementPromptOnRevealedBurnText(t *testing.T) {
	expire := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	f := newAckFixture(t, "")
	seedRevealedBurn(t, f, expire)
	beforeCount, beforeAt := disappearRows(t, f)

	f.snapshot.Store(revealedBurnSnapshot(expire, secretText, "self"))
	runAckFrames(t, f.client, ackFrameFor("self", true))
	if len(f.bot.edited) != 1 {
		t.Fatalf("acknowledging a revealed burn post sent %d edits, want one", len(f.bot.edited))
	}
	if body := editBody(f.bot.edited[0]); body != secretText {
		t.Errorf("after acknowledging body = %q, want the revealed text without the prompt", body)
	}
	if !f.promptHidden(t) {
		t.Error("prompt state was not persisted as hidden")
	}
	if f.reveals.Load() != 0 {
		t.Error("acknowledgement handling revealed the post again")
	}
	if count, at := disappearRows(t, f); count != beforeCount || at != beforeAt {
		t.Errorf("removal schedule changed: count=%d at=%d, want count=%d at=%d", count, at, beforeCount, beforeAt)
	}

	// Withdrawing the acknowledgement restores the instruction in front of the
	// same revealed text; it must not turn the body back into a placeholder.
	f.snapshot.Store(revealedBurnSnapshot(expire, secretText))
	runAckFrames(t, f.client, ackFrameFor("self", false))
	if len(f.bot.edited) != 2 {
		t.Fatalf("withdrawing sent %d edits in total, want two", len(f.bot.edited))
	}
	if body := editBody(f.bot.edited[1]); body != secretText+"\n\n"+promptLine {
		t.Errorf("restored body = %q, want the revealed text followed by the prompt", body)
	}
	if f.promptHidden(t) {
		t.Error("prompt state was not persisted as shown")
	}
	if count, at := disappearRows(t, f); count != beforeCount || at != beforeAt {
		t.Errorf("restoring the prompt changed the removal schedule: count=%d at=%d", count, at)
	}
}

// Every precondition is required. A missing or expired deadline, a timer that
// would outlive the reveal, files, a quote, or a snapshot that is no longer
// revealed all leave the instruction in place instead of guessing.
func TestAcknowledgementPromptRevealedBurnGuards(t *testing.T) {
	now := time.Now()
	expire := now.Add(time.Hour).Truncate(time.Millisecond)
	later := expire.Add(time.Hour)
	past := now.Add(-time.Minute).Truncate(time.Millisecond)
	pointer := func(v time.Time) *time.Time { return &v }

	cases := []struct {
		name     string
		row      *time.Time
		snapshot ackSnapshot
	}{
		{"missing revealed body", pointer(expire), revealedBurnSnapshot(expire, "", "self")},
		{"blank revealed body", pointer(expire), revealedBurnSnapshot(expire, "  ", "self")},
		{"expired viewer copy", pointer(expire), revealedBurnSnapshot(past, secretText, "self")},
		{"missing removal row", nil, revealedBurnSnapshot(expire, secretText, "self")},
		{"expired removal row", pointer(past), revealedBurnSnapshot(expire, secretText, "self")},
		{"timer outlives the reveal", pointer(later), revealedBurnSnapshot(expire, secretText, "self")},
		{"snapshot no longer revealed", pointer(expire), ackSnapshot{asking: true, author: "alice", burn: true, actors: []string{"self"}}},
		{"attachment", pointer(expire), ackSnapshot{asking: true, author: "alice", message: secretText, burn: true,
			files: []string{"file1"}, actors: []string{"self"}, revealExpireAt: expire.UnixMilli()}},
		{"quoted reply", pointer(expire), ackSnapshot{asking: true, author: "alice", message: secretText, burn: true,
			quotedTo: "parent", actors: []string{"self"}, revealExpireAt: expire.UnixMilli()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAckFixture(t, "")
			f.source.Metadata = &MessageMetadata{AckPromptTracked: true, AckPromptUnsafe: true, AckPromptRevealedBurn: true}
			mustAck(t, f.bridge.DB.Message.Update(t.Context(), f.source))
			if tc.row != nil {
				insertDisappear(t, f, *tc.row)
			}
			beforeCount, beforeAt := disappearRows(t, f)
			f.snapshot.Store(tc.snapshot)
			runAckFrames(t, f.client, ackFrameFor("self", true))
			if len(f.bot.edited) != 0 {
				t.Fatalf("edited despite an unmet guard: %v", f.bot.edited)
			}
			if f.promptHidden(t) {
				t.Error("prompt state changed although no edit was sent")
			}
			if count, at := disappearRows(t, f); count != beforeCount || at != beforeAt {
				t.Errorf("guard changed the removal schedule: count=%d at=%d", count, at)
			}
		})
	}
}

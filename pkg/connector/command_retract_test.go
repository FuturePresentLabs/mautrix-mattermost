package connector

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// retractions records the redactions the client asks for, and where.
type retractions struct {
	mu    sync.Mutex
	rooms []id.RoomID
	ids   []id.EventID
	err   error
}

func (r *retractions) fn(_ context.Context, roomID id.RoomID, eventID id.EventID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rooms = append(r.rooms, roomID)
	r.ids = append(r.ids, eventID)
	return r.err
}

// commandCall sends body as a Matrix message whose portal names a room, so a
// retraction can be checked against the room it targets.
func commandCall(body string) *bridgev2.MatrixMessage {
	msg := makeMatrixMessage("ch1", event.MsgText, body)
	msg.Event = &event.Event{ID: id.EventID("$evt")}
	msg.Portal.MXID = "!room:example.com"
	return msg
}

// A command's text is an instruction, not a message. Once Mattermost has run
// it, the Matrix copy is redacted so it does not sit in the room as a bare line
// above the output the command itself produced. The retraction waits until the
// command has been saved: redacted sooner, its own redaction would arrive
// before there is anything to match it to.
func TestASuccessfulCommandTakesBackItsInvocation(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	var red retractions
	c.redactCommandFn = red.fn

	resp, err := c.HandleMatrixMessage(context.Background(), commandCall("/meet start"))
	if err != nil {
		t.Fatal(err)
	}
	if len(red.ids) != 0 {
		t.Fatalf("took the invocation back before the command was saved: %v", red.ids)
	}
	if resp.PostSave == nil {
		t.Fatal("nothing retracts the invocation, so it stays in the room")
	}
	resp.PostSave(context.Background(), resp.DB)

	if len(red.ids) != 1 || red.ids[0] != "$evt" {
		t.Fatalf("retracted %v, want exactly the invocation event", red.ids)
	}
	if red.rooms[0] != "!room:example.com" {
		t.Fatalf("retracted in %q, want the portal room", red.rooms[0])
	}
	if len(s.executed) != 1 || resp.DB.ID != "cmd:$evt" {
		t.Fatalf("executed %v, saved as %q", s.executed, resp.DB.ID)
	}
}

// A command that failed must stay visible: it is the only record of what was
// attempted, and the error is attached to it.
func TestAFailedCommandKeepsItsInvocation(t *testing.T) {
	s := &commandServer{executeFn: func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"You do not have permission"}`))
	}}
	c, _ := commandClient(t, s)
	var red retractions
	c.redactCommandFn = red.fn

	resp, err := c.HandleMatrixMessage(context.Background(), commandCall("/away"))
	if err == nil {
		t.Fatal("expected the Mattermost error")
	}
	if resp != nil && resp.PostSave != nil {
		t.Error("a failed command was scheduled for retraction")
	}
	if len(red.ids) != 0 {
		t.Fatalf("retracted %v, want a failed command left in place", red.ids)
	}
}

// Ordinary text is a message, not an instruction, and is never taken back.
func TestOrdinaryTextIsNeverRetracted(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	var red retractions
	c.redactCommandFn = red.fn

	resp, err := c.HandleMatrixMessage(context.Background(), commandCall("hello there"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.PostSave != nil {
		t.Error("ordinary text was scheduled for retraction")
	}
	if len(red.ids) != 0 {
		t.Fatalf("retracted %v", red.ids)
	}
}

// A retraction that fails never fails the command: Mattermost already ran it
// and that cannot be undone.
func TestAFailedRetractionLeavesTheCommandAlone(t *testing.T) {
	s := &commandServer{}
	c, _ := commandClient(t, s)
	var red retractions
	red.err = errors.New("room is gone")
	c.redactCommandFn = red.fn

	resp, err := c.HandleMatrixMessage(context.Background(), commandCall("/meet start"))
	if err != nil {
		t.Fatalf("a failed retraction failed the command: %v", err)
	}
	if resp.PostSave == nil {
		t.Fatal("nothing retracts the invocation")
	}
	resp.PostSave(context.Background(), resp.DB)
	if len(red.ids) != 1 {
		t.Fatalf("retracted %v", red.ids)
	}
}

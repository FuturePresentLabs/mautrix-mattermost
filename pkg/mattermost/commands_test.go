package mattermost

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListCommandsMergesCustomAndToleratesForbidden(t *testing.T) {
	for _, customStatus := range []int{http.StatusOK, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v4/teams/t1/commands/autocomplete":
				_, _ = w.Write([]byte(`[{"trigger":"away"},{"trigger":"meet"}]`))
			case "/api/v4/commands":
				if r.URL.Query().Get("team_id") != "t1" {
					t.Errorf("custom list team = %q", r.URL.Query().Get("team_id"))
				}
				w.WriteHeader(customStatus)
				if customStatus == http.StatusOK {
					_, _ = w.Write([]byte(`[{"trigger":"hidden"}]`))
				} else {
					_, _ = w.Write([]byte(`{"id":"api.context.permissions.app_error","status_code":403}`))
				}
			default:
				t.Errorf("unexpected %s", r.URL.Path)
			}
		}))
		cmds, err := ListCommands(srv.URL, "tok", "t1")
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if customStatus == http.StatusOK {
			want = 3
		}
		if len(cmds) != want {
			t.Fatalf("status %d: %d commands, want %d", customStatus, len(cmds), want)
		}
	}
}

func TestListCommandsFailsOnOtherErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/commands" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	if _, err := ListCommands(srv.URL, "tok", "t1"); err == nil {
		t.Fatal("a 500 from the custom list was swallowed")
	}
}

func TestExecuteCommandRequestAndError(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/commands/execute" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["command"] == "/nope" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"id":"api.command.execute_command.not_found.app_error","message":"Command with a trigger of 'nope' not found.","status_code":404}`))
			return
		}
		_, _ = w.Write([]byte(`{"response_type":"ephemeral","goto_location":"https://mm.example.com/x"}`))
	}))
	defer srv.Close()
	resp, err := ExecuteCommand(srv.URL, "tok", "ch", "t1", "root", "/away")
	if err != nil || resp.GotoLocation != "https://mm.example.com/x" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if body["channel_id"] != "ch" || body["team_id"] != "t1" || body["root_id"] != "root" || body["parent_id"] != "root" {
		t.Fatalf("body = %v", body)
	}
	_, err = ExecuteCommand(srv.URL, "tok", "ch", "", "", "/nope")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Message != "Command with a trigger of 'nope' not found." {
		t.Fatalf("err = %v", err)
	}
	if _, ok := body["team_id"]; ok {
		t.Fatal("empty team sent")
	}
	if _, ok := body["root_id"]; ok {
		t.Fatal("empty root sent")
	}
}

func TestEphemeralMessageEvent(t *testing.T) {
	c := &Client{Events: make(chan any, 1)}
	c.parseEvent([]byte(`{"event":"ephemeral_message","data":{"post":"{\"id\":\"p\",\"channel_id\":\"ch\",\"root_id\":\"r\",\"message\":\"You are now away\"}"}}`))
	var ev *EphemeralEvent
	var ok bool
	select {
	case got := <-c.Events:
		ev, ok = got.(*EphemeralEvent)
	default:
	}
	if !ok || ev.Post.ChannelID != "ch" || ev.Post.RootID != "r" || ev.Post.Message != "You are now away" {
		t.Fatalf("event = %#v", ev)
	}
}

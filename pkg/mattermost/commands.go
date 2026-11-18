package mattermost

import (
	"errors"
	"net/http"
	"net/url"
)

// Command is a slash command available in a team.
type Command struct {
	Trigger     string `json:"trigger"`
	DisplayName string `json:"display_name"`
	Description string `json:"auto_complete_desc"`
	Hint        string `json:"auto_complete_hint"`
}

// CommandResponse is Mattermost's answer to running a command. Replies meant
// only for the user ("ephemeral") are not in it; the server sends them as an
// ephemeral_message websocket event.
type CommandResponse struct {
	ResponseType string `json:"response_type"`
	Text         string `json:"text"`
	GotoLocation string `json:"goto_location"`
}

// ListCommands returns the triggers the user can run in a team: the
// autocomplete list (built-in and plugin commands), plus the team's custom
// commands when the user may list them (custom commands hidden from
// autocomplete appear only there).
func ListCommands(serverURL, token, teamID string) ([]Command, error) {
	var auto []Command
	if err := doGet(serverURL, token, "/api/v4/teams/"+teamID+"/commands/autocomplete", &auto); err != nil {
		return nil, err
	}
	var custom []Command
	err := doGet(serverURL, token, "/api/v4/commands?team_id="+url.QueryEscape(teamID), &custom)
	var httpErr *HTTPError
	if err != nil && !(errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusForbidden) {
		return nil, err
	}
	return append(auto, custom...), nil
}

// ExecuteCommand runs a slash command ("/away", "/meet start") in a channel,
// as typing it in Mattermost does. rootID runs it in a thread.
func ExecuteCommand(serverURL, token, channelID, teamID, rootID, command string) (*CommandResponse, error) {
	body := map[string]any{"channel_id": channelID, "command": command}
	if teamID != "" {
		body["team_id"] = teamID
	}
	if rootID != "" {
		body["root_id"] = rootID
		body["parent_id"] = rootID
	}
	var out CommandResponse
	if err := doPost(serverURL, token, "/api/v4/commands/execute", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

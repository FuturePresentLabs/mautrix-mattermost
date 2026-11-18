package connector

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// One login per Mattermost team.
//
// Each login bridges one team's channels, so a Matrix client that groups and
// filters by account (Beeper's inbox toggles, for example) shows each team as
// its own account. DMs and group DMs belong to no team; exactly one of a
// user's logins carries them (UserLoginMetadata.DMs). It is the user's first
// login unless they pick another with the dm-team command.
//
// Logins made before teams were per login have no TeamID. On connect such a
// login is pinned to the user's first team (teamForLegacyLogin) and keeps the
// DMs; rooms of other teams' channels are removed from it, and come back under
// that team's login when the user adds one.

// teamStep is the login step that picks the team. Every login flow verifies
// its credentials, then hands them to begin; the flow's SubmitUserInput calls
// submit while a choice is pending.
type teamStep struct {
	pendingLogin *verifiedLogin
	// finishFn replaces finishTeamLogin in tests.
	finishFn func(v *verifiedLogin, team *mattermost.Team) (*bridgev2.LoginStep, error)
	// perTeamFn overrides network.team_logins in tests.
	perTeamFn func() bool
}

// perTeam reports network.team_logins. Without a bridge config (tests that
// build bare steps) it is on, as before logins could cover the account.
func (s *teamStep) perTeam(user *bridgev2.User) bool {
	if s.perTeamFn != nil {
		return s.perTeamFn()
	}
	if cfg := bridgeConfig(userBridge(user)); cfg != nil {
		return cfg.TeamLogins
	}
	return true
}

func (s *teamStep) finish(ctx context.Context, user *bridgev2.User, v *verifiedLogin, team *mattermost.Team) (*bridgev2.LoginStep, error) {
	if s.finishFn != nil {
		return s.finishFn(v, team)
	}
	return finishTeamLogin(ctx, user, v, team)
}

// verifiedLogin is a credential that Mattermost accepted, waiting for a team.
type verifiedLogin struct {
	serverURL, token, userID, username string
	teams                              []*mattermost.Team
}

const teamFieldID = "team"

func (s *teamStep) pending() bool { return s.pendingLogin != nil }

// begin finishes the login with the user's only team, or asks which team.
func (s *teamStep) begin(ctx context.Context, user *bridgev2.User, serverURL, token, userID, username string) (*bridgev2.LoginStep, error) {
	teams, err := mattermost.GetTeams(serverURL, token, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to list your teams: %w", err)
	}
	if len(teams) == 0 {
		return nil, fmt.Errorf("%s is not a member of any team", username)
	}
	v := &verifiedLogin{serverURL: serverURL, token: token, userID: userID, username: username, teams: teams}
	if !s.perTeam(user) {
		return s.finish(ctx, user, v, nil) // one login for the account
	}
	if len(teams) == 1 {
		return s.finish(ctx, user, v, teams[0])
	}
	s.pendingLogin = v
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "com.bostrot.mattermost.choose_team",
		Instructions: "Each team is a separate login. Choose the team to bridge; log in again for each other team.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{{
				Type:    bridgev2.LoginInputFieldTypeSelect,
				ID:      teamFieldID,
				Name:    "Team",
				Options: teamOptions(teams),
			}},
		},
	}, nil
}

// submit finishes the login with the chosen team.
func (s *teamStep) submit(ctx context.Context, user *bridgev2.User, input map[string]string) (*bridgev2.LoginStep, error) {
	v := s.pendingLogin
	team := teamForOption(v.teams, input[teamFieldID])
	if team == nil {
		return nil, fmt.Errorf("unknown team %q", input[teamFieldID])
	}
	s.pendingLogin = nil
	return s.finish(ctx, user, v, team)
}

// teamLabel is how a team is offered and shown: its display name, with the
// team's handle added when two teams share a display name.
func teamLabel(team *mattermost.Team, all []*mattermost.Team) string {
	name := team.DisplayName
	if name == "" {
		return team.Name
	}
	for _, other := range all {
		if other.ID != team.ID && other.DisplayName == name {
			return name + " (" + team.Name + ")"
		}
	}
	return name
}

func teamOptions(teams []*mattermost.Team) []string {
	out := make([]string, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamLabel(t, teams))
	}
	return out
}

// teamForOption accepts a label, a team handle or a team ID, case-insensitively.
func teamForOption(teams []*mattermost.Team, choice string) *mattermost.Team {
	choice = strings.TrimSpace(choice)
	for _, t := range teams {
		if strings.EqualFold(teamLabel(t, teams), choice) || strings.EqualFold(t.Name, choice) || t.ID == choice {
			return t
		}
	}
	return nil
}

// loginKey identifies a Mattermost account on a server.
func loginKey(serverURL, userID string) string {
	return strings.TrimRight(serverURL, "/") + "|" + userID
}

func sameAccount(meta *UserLoginMetadata, serverURL, userID string) bool {
	return meta != nil && loginKey(meta.ServerURL, meta.UserID) == loginKey(serverURL, userID)
}

// teamLoginID is the login ID for an account and team. A login made before
// teams were per login keeps its ID (the user ID), so it is not duplicated
// when its owner logs in again.
func teamLoginID(existing []*bridgev2.UserLogin, serverURL, userID, teamID string) networkid.UserLoginID {
	if l := findTeamLogin(existing, serverURL, userID, teamID); l != nil {
		return l.ID
	}
	return networkid.UserLoginID(userID + "_" + teamID)
}

// findTeamLogin is the user's login for the account and team: one pinned to
// the team, else a not-yet-pinned older login (which then takes this team).
func findTeamLogin(existing []*bridgev2.UserLogin, serverURL, userID, teamID string) *bridgev2.UserLogin {
	var legacy *bridgev2.UserLogin
	for _, l := range existing {
		meta, _ := l.Metadata.(*UserLoginMetadata)
		if !sameAccount(meta, serverURL, userID) {
			continue
		}
		if meta.TeamID == teamID {
			return l
		}
		if meta.TeamID == "" && legacy == nil {
			legacy = l
		}
	}
	return legacy
}

// dmOwnerFor reports whether a login being created should carry the DMs: it
// keeps its own setting when it exists, otherwise it does when none of the
// user's other logins for the account does (so the first login gets them).
func dmOwnerFor(existing []*bridgev2.UserLogin, self networkid.UserLoginID, serverURL, userID string) bool {
	for _, l := range existing {
		meta, _ := l.Metadata.(*UserLoginMetadata)
		if !sameAccount(meta, serverURL, userID) {
			continue
		}
		if l.ID == self {
			return meta.DMs || meta.TeamID == ""
		}
	}
	for _, l := range existing {
		meta, _ := l.Metadata.(*UserLoginMetadata)
		if sameAccount(meta, serverURL, userID) && (meta.DMs || meta.TeamID == "") {
			return false
		}
	}
	return true
}

// finishTeamLogin creates or updates the login for the team, or with team nil
// the login for the whole account (teamtoggle.go).
func finishTeamLogin(ctx context.Context, user *bridgev2.User, v *verifiedLogin, team *mattermost.Team) (*bridgev2.LoginStep, error) {
	existing := user.GetUserLogins()
	var meta *UserLoginMetadata
	var id networkid.UserLoginID
	var label string
	if team == nil {
		id, meta = accountLogin(existing, v)
		label = meta.Username
	} else {
		id = teamLoginID(existing, v.serverURL, v.userID, team.ID)
		label = teamLabel(team, v.teams)
		meta = &UserLoginMetadata{
			UserID:    v.userID,
			Username:  v.username,
			Token:     v.token,
			ServerURL: v.serverURL,
			TeamID:    team.ID,
			TeamName:  label,
			DMs:       dmOwnerFor(existing, id, v.serverURL, v.userID),
		}
	}
	ul, err := user.NewLogin(ctx, &database.UserLogin{
		ID:            id,
		RemoteName:    label,
		RemoteProfile: teamRemoteProfile(v.username, label),
		Metadata:      meta,
	}, &bridgev2.NewLoginParams{
		LoadUserLogin: func(ctx context.Context, login *bridgev2.UserLogin) error {
			login.Client = newMattermostClient(login, meta)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create login: %w", err)
	}
	ul.Client.Connect(ul.Log.WithContext(ctx))
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeComplete,
		StepID:       "com.bostrot.mattermost.complete",
		Instructions: fmt.Sprintf("Logged in as %s to %s", v.username, label),
		CompleteParams: &bridgev2.LoginCompleteParams{
			UserLoginID: ul.ID,
			UserLogin:   ul,
		},
	}, nil
}

func newMattermostClient(login *bridgev2.UserLogin, meta *UserLoginMetadata) *MattermostClient {
	return &MattermostClient{
		UserLogin: login,
		Username:  meta.Username,
		Token:     meta.Token,
		ServerURL: meta.ServerURL,
		UserID:    meta.UserID,
	}
}

// --- Channel ownership ---

// loginMeta is this login's metadata, nil in tests that build bare clients.
func (c *MattermostClient) loginMeta() *UserLoginMetadata {
	if c.UserLogin == nil || c.UserLogin.UserLogin == nil {
		return nil
	}
	meta, _ := c.UserLogin.Metadata.(*UserLoginMetadata)
	return meta
}

// teamScope is the team this login bridges, "" for all teams (a login not yet
// pinned, or a bare test client).
func (c *MattermostClient) teamScope() string {
	if meta := c.loginMeta(); meta != nil {
		return meta.TeamID
	}
	return ""
}

// ownsDMs reports whether this login bridges DMs and group DMs.
func (c *MattermostClient) ownsDMs() bool {
	meta := c.loginMeta()
	return meta == nil || meta.TeamID == "" || meta.DMs
}

// ownsChannel reports whether this login bridges the channel: a team channel
// of its team, or a DM or group DM when it carries the DMs.
func (c *MattermostClient) ownsChannel(ch *mattermost.Channel) bool {
	if ch == nil {
		return false
	}
	if ch.IsDM() || ch.IsGroupDM() {
		return c.ownsDMs()
	}
	return c.ownsTeam(ch.TeamID)
}

// ownsTeam reports whether this login bridges a team's channels: its own
// team, or for an account login a team switched on. A bare test client (no
// metadata) bridges every team.
func (c *MattermostClient) ownsTeam(teamID string) bool {
	meta := c.loginMeta()
	if meta == nil {
		return true
	}
	if meta.TeamID != "" {
		return teamID == meta.TeamID
	}
	if !c.accountLogins() {
		return true // a legacy login waiting to be pinned
	}
	return teamID != "" && meta.EnabledTeams[teamID]
}

// ownsPostedChannel decides from a posted event's own metadata, without a
// lookup: the channel type and team come with the event.
func (c *MattermostClient) ownsPostedChannel(channelType, teamID string) bool {
	if channelType == "D" || channelType == "G" {
		return c.ownsDMs()
	}
	return c.ownsTeam(teamID)
}

// teamForLegacyLogin is the team a not-yet-pinned login is pinned to: the
// first of the user's teams by display name (then ID), so the choice does not
// depend on the order the server lists them.
func teamForLegacyLogin(teams []*mattermost.Team) *mattermost.Team {
	if len(teams) == 0 {
		return nil
	}
	sorted := append([]*mattermost.Team(nil), teams...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := strings.ToLower(sorted[i].DisplayName), strings.ToLower(sorted[j].DisplayName)
		if a != b {
			return a < b
		}
		return sorted[i].ID < sorted[j].ID
	})
	return sorted[0]
}

// teamRemoteProfile is how a team login describes itself to the client.
func teamRemoteProfile(username, teamLabel string) status.RemoteProfile {
	return status.RemoteProfile{Username: username, Name: teamLabel}
}

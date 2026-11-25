// Package connector implements the bridgev2 network interface for Mattermost.
package connector

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"go.mau.fi/util/configupgrade"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// MattermostConnector is the top-level bridgev2.NetworkConnector for Mattermost.
type MattermostConnector struct {
	Bridge *bridgev2.Bridge
	Config *Config
}

var _ bridgev2.NetworkConnector = (*MattermostConnector)(nil)

func (m *MattermostConnector) Init(bridge *bridgev2.Bridge) {
	m.Bridge = bridge
	registerCommands(bridge)
}

func (m *MattermostConnector) Start(ctx context.Context) error {
	if m.Config != nil {
		if _, err := parseDisplayNames(m.Config.DisplayNames); err != nil {
			return fmt.Errorf("network.display_names: %w", err)
		}
		if _, err := parseDMStatus(m.Config.DMStatus); err != nil {
			return fmt.Errorf("network.dm_status: %w", err)
		}
	}
	return nil
}

func (m *MattermostConnector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "Mattermost",
		NetworkURL:       "https://mattermost.com",
		NetworkID:        "mattermost",
		BeeperBridgeType: "github.com/bostrot/mautrix-mattermost",
		DefaultPort:      29345,
	}
}

func (m *MattermostConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal: func() any { return &PortalMetadata{} },
		UserLogin: func() any {
			return &UserLoginMetadata{}
		},
		Message: func() any { return &MessageMetadata{} },
	}
}

// MessageMetadata is the connector's own state for one bridged post. It holds
// only presentation state: whether the acknowledgement prompt is currently
// shown. The post's text is never copied here, so no additional message
// plaintext is retained, and a burn-on-read reveal is never stored twice.
type MessageMetadata struct {
	// AckPromptHidden says whether the instruction is currently part of the
	// event's visible body.
	AckPromptHidden bool `json:"ack_prompt_hidden,omitempty"`
	// AckPromptTracked distinguishes a row written by this bridge from one
	// converted before the state existed. Only tracked rows may be edited, so
	// legacy messages are never rewritten on a guess.
	AckPromptTracked bool `json:"ack_prompt_tracked,omitempty"`
	// AckPromptUnsafe marks a body the bridge cannot rebuild faithfully from a
	// later post snapshot: media captions, quoted replies, link previews, and
	// revealed burn-on-read text. It is recorded at conversion because a later
	// snapshot can omit the files or props that made rebuild unsafe.
	AckPromptUnsafe bool `json:"ack_prompt_unsafe,omitempty"`
	// AckPromptRevealedBurn marks the one unsafe body that has a separate,
	// strictly validated rebuild path: the text this account revealed from a
	// burn-on-read post. The reveal path alone sets it, so media, quotes and
	// previews can never fall through that guard.
	AckPromptRevealedBurn bool `json:"ack_prompt_revealed_burn,omitempty"`
	// MediaFileID identifies the Mattermost file this row was converted from.
	// It is identity only: no filename, caption, or other content is stored.
	// An empty value means the row is not known to be media, except for legacy
	// rows that predate this field and are flagged unsafe instead.
	MediaFileID string `json:"media_file_id,omitempty"`
}

// CopyFrom merges metadata from an edit part into the row being edited.
func (m *MessageMetadata) CopyFrom(other any) {
	if o, ok := other.(*MessageMetadata); ok && o != nil {
		m.AckPromptHidden = o.AckPromptHidden
		m.AckPromptTracked = o.AckPromptTracked
		m.AckPromptUnsafe = o.AckPromptUnsafe
		m.AckPromptRevealedBurn = o.AckPromptRevealedBurn
		m.MediaFileID = o.MediaFileID
	}
}

func (m *MattermostConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	// Start the expiry worker even when scheduling new notices is disabled,
	// so it can finish processing notices already stored in the database.
	return &bridgev2.NetworkGeneralCapabilities{DisappearingMessages: true}
}

// GetConfig returns the network section of the config. upgradeConfig has to copy every
// field explicitly: the framework rebuilds the file from the example, so anything the
// upgrader does not copy is silently replaced by the example's default while the file
// still shows the user's value.
func (m *MattermostConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	if m.Config == nil {
		m.Config = &Config{}
	}
	return ExampleConfig, m.Config, configupgrade.SimpleUpgrader(upgradeConfig)
}

// upgradeConfig carries the user's network options over the example config.
func upgradeConfig(helper configupgrade.Helper) {
	helper.Copy(configupgrade.Bool, "presence", "enabled")
	helper.Copy(configupgrade.Str, "display_names")
	helper.Copy(configupgrade.Str, "dm_status")
	helper.Copy(configupgrade.Bool, "team_spaces")
	helper.Copy(configupgrade.Bool, "team_logins")
	helper.Copy(configupgrade.Bool, "mute_channels_on_create")
	helper.Copy(configupgrade.Str, "account_space_name")
	helper.Copy(configupgrade.Bool, "quoted_replies")
	helper.Copy(configupgrade.Bool, "expire_ephemeral_notices")
}

func (m *MattermostConnector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	login.Client = newMattermostClient(login, meta)
	return nil
}

func (m *MattermostConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{
		{
			Name:        "Login with Personal Access Token",
			Description: "Login using a Mattermost Personal Access Token (PAT) and server URL",
			ID:          "mm-login-pat",
		},
		{
			Name:        "Login with Email & Password",
			Description: "Login using your Mattermost email and password",
			ID:          "mm-login-password",
		},
		{
			Name:        "Login with Browser Cookie (SSO)",
			Description: "Login using the MMAUTHTOKEN cookie from your browser session (for SSO/GitLab login)",
			ID:          "mm-login-cookie",
		},
		{
			Name:        "Login in Browser Window (SSO)",
			Description: "Log in to Mattermost in a built-in browser window; the MMAUTHTOKEN cookie is collected automatically",
			ID:          "mm-login-webview",
		},
	}
}

func (m *MattermostConnector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case "mm-login-pat":
		return &PATLoginProcess{User: user}, nil
	case "mm-login-password":
		return &PasswordLoginProcess{User: user}, nil
	case "mm-login-cookie":
		return &CookieLoginProcess{User: user}, nil
	case "mm-login-webview":
		return &WebviewLoginProcess{CookieLoginProcess: CookieLoginProcess{User: user}}, nil
	default:
		return nil, fmt.Errorf("unknown login flow ID: %s", flowID)
	}
}

func (m *MattermostConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

// UserLoginMetadata stores persistent login credentials per user.
type UserLoginMetadata struct {
	UserID    string `json:"user_id"`
	Username  string `json:"username"`
	Token     string `json:"token"`
	ServerURL string `json:"server_url"`
	// TeamID is the team this login bridges (teamlogin.go); empty for a
	// login made before teams were per login, until it is pinned.
	TeamID string `json:"team_id,omitempty"`
	// TeamName is the team as shown to the user.
	TeamName string `json:"team_name,omitempty"`
	// DMs marks the one login of the account that bridges DMs and group DMs.
	DMs bool `json:"dms,omitempty"`
	// TeamIcon is the team icon last set as the login's avatar
	// ("<team id>_<last_team_icon_update>"), so it is uploaded once.
	TeamIcon string `json:"team_icon,omitempty"`
	// MutesSynced is set once the rooms that existed before mutes were
	// bridged have been muted where their channel is (mutes.go).
	MutesSynced bool `json:"mutes_synced,omitempty"`
	// EnabledTeams is the teams an account login (no TeamID) bridges, keyed
	// by team ID (teamtoggle.go). Teams start switched off.
	EnabledTeams map[string]bool `json:"enabled_teams,omitempty"`
	// SpaceName is the account space name last set, so it is set once per
	// change of network.account_space_name.
	SpaceName string `json:"space_name,omitempty"`
}

// --- PAT Login ---

// PATLoginProcess handles Personal Access Token-based login.
type PATLoginProcess struct {
	User *bridgev2.User
	team teamStep
}

var _ bridgev2.LoginProcessUserInput = (*PATLoginProcess)(nil)

func (p *PATLoginProcess) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "com.bostrot.mattermost.enter_pat",
		Instructions: "Enter your Mattermost server URL and Personal Access Token",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type: bridgev2.LoginInputFieldTypeUsername,
					ID:   "server_url",
					Name: "Server URL (e.g. https://mattermost.example.com)",
				},
				{
					Type: bridgev2.LoginInputFieldTypePassword,
					ID:   "token",
					Name: "Personal Access Token",
				},
			},
		},
	}, nil
}

func (p *PATLoginProcess) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if p.team.pending() {
		return p.team.submit(ctx, p.User, input)
	}
	serverURL := input["server_url"]
	token := input["token"]
	if serverURL == "" || token == "" {
		return nil, fmt.Errorf("server_url and token are required")
	}

	// Verify the token by fetching /users/me.
	userID, username, err := mattermost.GetSelf(serverURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to verify token: %w", err)
	}

	return p.team.begin(ctx, p.User, serverURL, token, userID, username)
}

func (p *PATLoginProcess) Cancel() {}

// --- Password Login ---

// PasswordLoginProcess handles email + password login.
type PasswordLoginProcess struct {
	User *bridgev2.User
	team teamStep
}

var _ bridgev2.LoginProcessUserInput = (*PasswordLoginProcess)(nil)

func (p *PasswordLoginProcess) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "com.bostrot.mattermost.enter_credentials",
		Instructions: "Enter your Mattermost server URL, email and password",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type: bridgev2.LoginInputFieldTypeUsername,
					ID:   "server_url",
					Name: "Server URL (e.g. https://mattermost.example.com)",
				},
				{
					Type: bridgev2.LoginInputFieldTypeEmail,
					ID:   "email",
					Name: "Email",
				},
				{
					Type: bridgev2.LoginInputFieldTypePassword,
					ID:   "password",
					Name: "Password",
				},
			},
		},
	}, nil
}

func (p *PasswordLoginProcess) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if p.team.pending() {
		return p.team.submit(ctx, p.User, input)
	}
	serverURL := input["server_url"]
	email := input["email"]
	password := input["password"]
	if serverURL == "" || email == "" || password == "" {
		return nil, fmt.Errorf("server_url, email and password are required")
	}

	token, err := mattermost.LoginWithPassword(serverURL, email, password)
	if err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}

	userID, username, err := mattermost.GetSelf(serverURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user info: %w", err)
	}

	return p.team.begin(ctx, p.User, serverURL, token, userID, username)
}

func (p *PasswordLoginProcess) Cancel() {}

// --- Cookie / SSO Login ---

// CookieLoginProcess handles SSO login via the MMAUTHTOKEN browser cookie.
// The MMAUTHTOKEN value is identical to a Bearer token and can be used directly
// with the Mattermost REST API. MMCSRF is only required for browser cookie-based
// requests and is not needed here.
type CookieLoginProcess struct {
	User *bridgev2.User
	team teamStep
}

var _ bridgev2.LoginProcessUserInput = (*CookieLoginProcess)(nil)

func (p *CookieLoginProcess) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: "com.bostrot.mattermost.enter_cookie",
		Instructions: "Log in to Mattermost in your browser (via SSO/GitLab), then open DevTools " +
			"(F12) → Application → Cookies → select your server → copy the value of MMAUTHTOKEN.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type: bridgev2.LoginInputFieldTypeUsername,
					ID:   "server_url",
					Name: "Server URL (e.g. https://mattermost.example.com)",
				},
				{
					Type: bridgev2.LoginInputFieldTypePassword,
					ID:   "token",
					Name: "MMAUTHTOKEN cookie value",
				},
			},
		},
	}, nil
}

func (p *CookieLoginProcess) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if p.team.pending() {
		return p.team.submit(ctx, p.User, input)
	}
	serverURL := input["server_url"]
	token := input["token"]
	if serverURL == "" || token == "" {
		return nil, fmt.Errorf("server_url and token are required")
	}

	// MMAUTHTOKEN is accepted as a Bearer token directly.
	userID, username, err := mattermost.GetSelf(serverURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to verify cookie token: %w", err)
	}

	return p.team.begin(ctx, p.User, serverURL, token, userID, username)
}

func (p *CookieLoginProcess) Cancel() {}

// --- Webview / SSO Login ---

// WebviewLoginProcess asks for the server URL, then has the client open the
// Mattermost login page in a webview and collect the HttpOnly MMAUTHTOKEN
// cookie itself, so SSO users don't have to copy it out of DevTools.
type WebviewLoginProcess struct {
	CookieLoginProcess
	serverURL string
}

var _ bridgev2.LoginProcessUserInput = (*WebviewLoginProcess)(nil)
var _ bridgev2.LoginProcessCookies = (*WebviewLoginProcess)(nil)
var _ bridgev2.LoginProcessWithOverride = (*WebviewLoginProcess)(nil)

// StartWithOverride skips the server URL prompt when re-authenticating an
// existing login, reusing the server URL stored in its metadata.
func (p *WebviewLoginProcess) StartWithOverride(ctx context.Context, override *bridgev2.UserLogin) (*bridgev2.LoginStep, error) {
	if meta, ok := override.Metadata.(*UserLoginMetadata); ok && meta.ServerURL != "" {
		return p.SubmitUserInput(ctx, map[string]string{"server_url": meta.ServerURL})
	}
	return p.Start(ctx)
}

func (p *WebviewLoginProcess) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       "com.bostrot.mattermost.enter_server_url",
		Instructions: "Enter your Mattermost server URL",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{
					Type: bridgev2.LoginInputFieldTypeURL,
					ID:   "server_url",
					Name: "Server URL (e.g. https://mattermost.example.com)",
				},
			},
		},
	}, nil
}

func (p *WebviewLoginProcess) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	if p.team.pending() {
		return p.team.submit(ctx, p.User, input)
	}
	step, serverURL, err := webviewCookieStep(input["server_url"])
	if err != nil {
		return nil, err
	}
	p.serverURL = serverURL
	return step, nil
}

func (p *WebviewLoginProcess) SubmitCookies(ctx context.Context, cookies map[string]string) (*bridgev2.LoginStep, error) {
	return p.CookieLoginProcess.SubmitUserInput(ctx, map[string]string{
		"server_url": p.serverURL,
		"token":      cookies["token"],
	})
}

// webviewCookieStep builds the cookies login step for the given server URL and
// returns the normalized URL.
func webviewCookieStep(rawURL string) (*bridgev2.LoginStep, string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", fmt.Errorf("invalid server URL %q", rawURL)
	}
	serverURL := strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeCookies,
		StepID:       "com.bostrot.mattermost.webview_cookie",
		Instructions: "Log in to Mattermost in the browser window",
		CookiesParams: &bridgev2.LoginCookiesParams{
			URL: serverURL + "/login",
			Fields: []bridgev2.LoginCookieField{{
				ID:       "token",
				Required: true,
				Sources: []bridgev2.LoginCookieFieldSource{{
					Type:         bridgev2.LoginCookieTypeCookie,
					Name:         "MMAUTHTOKEN",
					CookieDomain: u.Hostname(),
				}},
			}},
			// After login Mattermost lands on /<team>/channels/... or /<team>/messages/...
			WaitForURLPattern: "^" + regexp.QuoteMeta(serverURL) + "/[^/]+/(channels|messages)/",
		},
	}, serverURL, nil
}

// Config holds the bridge-specific configuration.
type Config struct {
	Presence PresenceConfig `yaml:"presence"`
	// DisplayNames picks how Mattermost users are named in Matrix (see
	// displaynames.go). Empty means "server".
	DisplayNames string `yaml:"display_names"`
	// DMStatus picks how the other person's status shows on 1:1 DM titles
	// (see dmstatus.go). Empty means "all".
	DMStatus string `yaml:"dm_status"`
	// TeamSpaces puts each team's channels in a Matrix space for the team
	// (see teams.go).
	TeamSpaces bool `yaml:"team_spaces"`
	// TeamLogins makes each team a separate login (teamlogin.go). Off: one
	// login per account, with each team switched on or off by the team
	// command (teamtoggle.go).
	TeamLogins bool `yaml:"team_logins"`
	// MuteChannelsOnCreate mutes a team channel's room when it is created,
	// so channels stay quiet and DMs notify.
	MuteChannelsOnCreate bool `yaml:"mute_channels_on_create"`
	// AccountSpaceName names an account login's own space (teamtoggle.go).
	// Empty keeps the framework's "Mattermost (<login>)".
	AccountSpaceName string `yaml:"account_space_name"`
	// QuotedReplies sends Matrix replies as Channel Reply plugin quoted
	// replies when the server has the plugin (quotes.go).
	QuotedReplies bool `yaml:"quoted_replies"`
	// ExpireEphemeralNotices expires copies of private Mattermost replies.
	ExpireEphemeralNotices bool `yaml:"expire_ephemeral_notices"`
}

const ExampleConfig = `# Mattermost Bridge Configuration

# Republish Mattermost user statuses (online / away / dnd / offline) as Matrix
# presence for each bridged ghost. Off by default: it costs one homeserver request
# per status change, and homeservers that drop presence gain nothing from it.
presence:
    enabled: false

# How Mattermost people are named in Matrix: their names and the text of
# @mention pills. Mentions are always sent and matched by username.
#   server    - as Mattermost shows them to you: your own display preference,
#               else the server's "Teammate Name Display" setting
#   username  - username (alice)
#   full_name - First Last, falling back to username
#   nickname  - nickname, then First Last, then username
# Changes apply to existing names at the next bridge restart.
display_names: server

# The other person's Mattermost status on 1:1 DM titles, with their custom
# status emoji after it and its text as the DM's topic. Title changes do not
# appear in the timeline. Channels and group DMs are not changed.
#   all  - every state: alice 🟢 (online), 🟡 (away), ⛔ (do not disturb), ⚪ (offline)
#   dnd  - only do not disturb: alice ⛔
#   off  - plain names
dm_status: all

# Put each Mattermost team's channels in a Matrix space named and pictured like
# the team. Turning it off moves the channels back out and removes the spaces.
team_spaces: true

# One login per Mattermost team instead of one login for the whole account.
# Off (the default): one login carries your DMs and the teams you switch on
# with the "team" bot command; teams start switched off. On: log in once per
# team; one of the logins carries the DMs (see the "dm-team" command).
team_logins: false

# Mute a team channel's room when it is created, so channels stay quiet while
# DMs notify. Unmute any room you want to hear from. Mutes you set in
# Mattermost are copied either way.
mute_channels_on_create: true

# Name of the account's own space, which holds the DMs and the team spaces,
# when one login covers the account. Empty keeps "Mattermost (<username>)".
account_space_name: Mattermost DMs

# Send replies that are not in a thread as quoted replies in the channel,
# when the Mattermost server has the Channel Reply plugin
# (com.github.mattermost-channel-reply). Without the plugin, or with this
# off, a reply goes to the thread of the message replied to.
quoted_replies: true

# Remove copies of private Mattermost replies (for example /gcal today) from the
# Matrix room after 24 hours. Mattermost has no event for dismissing them.
# Bridge status messages and ordinary bot DMs are not removed.
expire_ephemeral_notices: true
`

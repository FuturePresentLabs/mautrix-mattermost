package connector

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// One login for the whole account, teams switched on one by one.
//
// With network.team_logins off (the default), a login covers the account, as
// a Discord login does: it carries the DMs and group DMs, and bridges the
// channels of the teams the user switches on with the team command. Teams
// start off. Switching a team on creates its rooms (in the team's space when
// team spaces are on, muted when network.mute_channels_on_create is on);
// switching it off removes them, and the space once it is empty.

// spaceCleanupDelay is when a second sync runs after a team is switched off,
// to remove its space once the rooms in it are gone.
var spaceCleanupDelay = 30 * time.Second

// accountLogins reports whether logins cover the whole account (network.
// team_logins off). Without a bridge config, as in tests of bare clients, it
// is off, matching how those tests treat logins.
func (c *MattermostClient) accountLogins() bool {
	if c.accountLoginsFn != nil {
		return c.accountLoginsFn()
	}
	if cfg := bridgeConfig(c.bridge()); cfg != nil {
		return !cfg.TeamLogins
	}
	return false
}

func (c *MattermostClient) bridge() *bridgev2.Bridge {
	if c.UserLogin == nil {
		return nil
	}
	return c.UserLogin.Bridge
}

func userBridge(u *bridgev2.User) *bridgev2.Bridge {
	if u == nil {
		return nil
	}
	return u.Bridge
}

// bridgeConfig is the network config, nil without a bridge.
func bridgeConfig(br *bridgev2.Bridge) *Config {
	if br == nil {
		return nil
	}
	if conn, ok := br.Network.(*MattermostConnector); ok {
		return conn.Config
	}
	return nil
}

// accountLogin is the login ID and metadata for an account login: the user's
// existing account login when there is one (keeping its switched-on teams and
// state), else a new one with the user ID as its ID.
func accountLogin(existing []*bridgev2.UserLogin, v *verifiedLogin) (networkid.UserLoginID, *UserLoginMetadata) {
	meta := &UserLoginMetadata{UserID: v.userID, Username: v.username, Token: v.token, ServerURL: v.serverURL, DMs: true}
	for _, l := range existing {
		old, _ := l.Metadata.(*UserLoginMetadata)
		if sameAccount(old, v.serverURL, v.userID) && old.TeamID == "" {
			meta.EnabledTeams, meta.MutesSynced = old.EnabledTeams, old.MutesSynced
			return l.ID, meta
		}
	}
	return networkid.UserLoginID(v.userID), meta
}

// accountSpaceName is network.account_space_name ("" without a config).
func (c *MattermostClient) accountSpaceName() string {
	if c.accountSpaceNameFn != nil {
		return c.accountSpaceNameFn()
	}
	if cfg := bridgeConfig(c.bridge()); cfg != nil {
		return strings.TrimSpace(cfg.AccountSpaceName)
	}
	return ""
}

// syncAccountSpaceName names an account login's space after
// network.account_space_name, once per change. The framework names the space
// only when creating it.
func (c *MattermostClient) syncAccountSpaceName() {
	meta := c.loginMeta()
	name := c.accountSpaceName()
	if meta == nil || meta.TeamID != "" || !c.accountLogins() || name == "" || meta.SpaceName == name {
		return
	}
	if !c.setSpaceState(event.StateRoomName, &event.RoomNameEventContent{Name: name}) {
		return // retried on the next connect
	}
	meta.SpaceName = name
	c.saveLogin()
}

// setTeamEnabled switches a team on or off and reports whether that changed.
func (c *MattermostClient) setTeamEnabled(teamID string, on bool) bool {
	meta := c.loginMeta()
	if meta == nil || meta.EnabledTeams[teamID] == on {
		return false
	}
	if on {
		if meta.EnabledTeams == nil {
			meta.EnabledTeams = map[string]bool{}
		}
		meta.EnabledTeams[teamID] = true
	} else {
		delete(meta.EnabledTeams, teamID)
	}
	return true
}

func (c *MattermostClient) listTeams() ([]*mattermost.Team, error) {
	if c.listTeamsFn != nil {
		return c.listTeamsFn()
	}
	return mattermost.GetTeams(c.serverURL(), c.Token, c.UserID)
}

// describeTeams lists the account's teams with their state.
func (c *MattermostClient) describeTeams(teams []*mattermost.Team) string {
	sorted := append([]*mattermost.Team(nil), teams...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.ToLower(teamLabel(sorted[i], teams)) < strings.ToLower(teamLabel(sorted[j], teams))
	})
	var b strings.Builder
	b.WriteString("Your Mattermost teams:\n")
	for _, t := range sorted {
		state := "off"
		if c.ownsTeam(t.ID) {
			state = "**on**"
		}
		fmt.Fprintf(&b, "\n* %s: %s", teamLabel(t, teams), state)
	}
	b.WriteString("\n\nSwitch one with `team on <team>` or `team off <team>`. DMs are always bridged.")
	return b.String()
}

// runTeamCommand handles "team", "team on <team>" and "team off <team>" for
// an account login. It returns the reply.
func (c *MattermostClient) runTeamCommand(args []string) string {
	teams, err := c.listTeams()
	if err != nil {
		return fmt.Sprintf("Could not list your teams: %v", err)
	}
	if len(args) == 0 {
		return c.describeTeams(teams)
	}
	verb := strings.ToLower(args[0])
	if (verb != "on" && verb != "off") || len(args) < 2 {
		return "Usage: `team`, `team on <team>` or `team off <team>`."
	}
	team := teamForOption(teams, strings.Join(args[1:], " "))
	if team == nil {
		return fmt.Sprintf("No team %q. %s", strings.Join(args[1:], " "), c.describeTeams(teams))
	}
	label := teamLabel(team, teams)
	if !c.setTeamEnabled(team.ID, verb == "on") {
		return fmt.Sprintf("**%s** is already %s.", label, verb)
	}
	c.saveLogin()
	c.resync()
	if verb == "on" {
		return fmt.Sprintf("**%s** is on. Its channels appear in the next minute.", label)
	}
	c.afterDelay(spaceCleanupDelay, c.resync) // removes the space once empty
	return fmt.Sprintf("**%s** is off. Its rooms are being removed.", label)
}

func (c *MattermostClient) resync() {
	if c.resyncFn != nil {
		c.resyncFn()
		return
	}
	go c.syncChannels()
}

func (c *MattermostClient) afterDelay(d time.Duration, f func()) {
	if c.afterDelayFn != nil {
		c.afterDelayFn(d, f)
		return
	}
	time.AfterFunc(d, f)
}

// The team command switches a team on or off for an account login.
var cmdTeam = &commands.FullHandler{
	Func: fnTeam,
	Name: "team",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "List your Mattermost teams, or switch one on or off.",
		Args:        "[on|off <team>]",
	},
	RequiresLogin: true,
}

func fnTeam(ce *commands.Event) {
	for _, l := range ce.User.GetUserLogins() {
		meta, _ := l.Metadata.(*UserLoginMetadata)
		c, ok := l.Client.(*MattermostClient)
		if meta == nil || meta.TeamID != "" || !ok {
			continue
		}
		if !c.accountLogins() {
			break
		}
		ce.Reply("%s", c.runTeamCommand(ce.Args))
		return
	}
	ce.Reply("Teams are switched on and off only when one login covers your account (network.team_logins: false). With a login per team, log in again for each team.")
}

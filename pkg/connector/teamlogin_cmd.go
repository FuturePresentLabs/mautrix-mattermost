package connector

import (
	"fmt"
	"strings"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
)

// dm-team shows or changes which of the user's team logins bridges DMs and
// group DMs (see teamlogin.go). Changing it moves the DM rooms: the old login
// removes its copies and the new one creates them, with history re-imported.
var cmdDMTeam = &commands.FullHandler{
	Func: fnDMTeam,
	Name: "dm-team",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Show or choose which team's login carries your Mattermost DMs and group DMs.",
		Args:        "[team]",
	},
	RequiresLogin: true,
}

func registerCommands(br *bridgev2.Bridge) {
	if proc, ok := br.Commands.(*commands.Processor); ok {
		proc.AddHandlers(cmdDMTeam, cmdTeam)
	}
}

func fnDMTeam(ce *commands.Event) {
	logins := ce.User.GetUserLogins()
	if len(ce.Args) == 0 {
		ce.Reply("%s", describeDMTeam(logins))
		return
	}
	target, changed, err := chooseDMLogin(logins, strings.Join(ce.Args, " "))
	if err != nil {
		ce.Reply("%s", err.Error())
		return
	}
	if len(changed) == 0 {
		ce.Reply("DMs are already in **%s**.", loginTeamName(target))
		return
	}
	for _, l := range changed {
		if err := l.Save(ce.Ctx); err != nil {
			ce.Reply("Could not save %s: %v", loginTeamName(l), err)
			return
		}
	}
	// Each changed login resyncs: the old DM login drops its DM rooms, the new
	// one creates them.
	for _, l := range changed {
		if c, ok := l.Client.(*MattermostClient); ok {
			go c.syncChannels()
		}
	}
	ce.Reply("DMs and group DMs now go to **%s**. Their rooms move over in the next minute; history is re-imported.", loginTeamName(target))
}

// teamLogins is the user's Mattermost logins that have a team.
func teamLogins(logins []*bridgev2.UserLogin) []*bridgev2.UserLogin {
	var out []*bridgev2.UserLogin
	for _, l := range logins {
		if meta, ok := l.Metadata.(*UserLoginMetadata); ok && meta.TeamID != "" {
			out = append(out, l)
		}
	}
	return out
}

func loginTeamName(l *bridgev2.UserLogin) string {
	meta, _ := l.Metadata.(*UserLoginMetadata)
	if meta == nil {
		return string(l.ID)
	}
	if meta.TeamName != "" {
		return meta.TeamName
	}
	return meta.TeamID
}

func describeDMTeam(logins []*bridgev2.UserLogin) string {
	tl := teamLogins(logins)
	if len(tl) == 0 {
		return "You have no Mattermost team logins yet."
	}
	var b strings.Builder
	b.WriteString("Your Mattermost team logins:\n")
	for _, l := range tl {
		meta := l.Metadata.(*UserLoginMetadata)
		mark := ""
		if meta.DMs {
			mark = " (DMs and group DMs)"
		}
		fmt.Fprintf(&b, "* %s%s\n", loginTeamName(l), mark)
	}
	b.WriteString("\nUse `dm-team <team>` to move the DMs to another team's login.")
	return b.String()
}

// chooseDMLogin picks the login for the named team and returns the logins
// whose DM setting changes (the target, and any other login of the same
// account that had the DMs). Only one login per account carries DMs.
func chooseDMLogin(logins []*bridgev2.UserLogin, choice string) (*bridgev2.UserLogin, []*bridgev2.UserLogin, error) {
	choice = strings.TrimSpace(choice)
	var target *bridgev2.UserLogin
	var names []string
	for _, l := range teamLogins(logins) {
		meta := l.Metadata.(*UserLoginMetadata)
		names = append(names, loginTeamName(l))
		if strings.EqualFold(meta.TeamName, choice) || meta.TeamID == choice {
			if target != nil {
				return nil, nil, fmt.Errorf("more than one login matches %q; use the team ID", choice)
			}
			target = l
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("no team login named %q (you have: %s)", choice, strings.Join(names, ", "))
	}
	tm := target.Metadata.(*UserLoginMetadata)
	var changed []*bridgev2.UserLogin
	for _, l := range teamLogins(logins) {
		meta := l.Metadata.(*UserLoginMetadata)
		if !sameAccount(meta, tm.ServerURL, tm.UserID) {
			continue
		}
		want := l == target
		if meta.DMs != want {
			meta.DMs = want
			changed = append(changed, l)
		}
	}
	return target, changed, nil
}

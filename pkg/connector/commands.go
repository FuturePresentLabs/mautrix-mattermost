package connector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Slash commands from Matrix.
//
// A text message whose first word is "/<trigger>" for a command the user can
// run in Mattermost is run there (POST /commands/execute) instead of posted,
// as typing it in Mattermost does. Anything else starting with "/" is posted
// as text, so "/usr/bin is full" is not swallowed; "//text" posts "/text".
//
// The command's message stays in the Matrix room as the user's record. It has
// no Mattermost post, so it is saved under a "cmd:" ID that edits, deletes,
// reactions and replies recognise. Replies only the user can see arrive as
// ephemeral_message events and are shown as notices from the bridge bot.

const commandMessagePrefix = "cmd:"

// commandListTTL bounds how long a team's command list is trusted; a trigger
// missing from a list older than commandListRefresh triggers one refetch, so
// a newly installed plugin works without waiting out the TTL.
const (
	commandListTTL     = 10 * time.Minute
	commandListRefresh = time.Minute
)

var commandWord = regexp.MustCompile(`^/([A-Za-z0-9_.\-]+)(?:\s|$)`)

type commandCache struct {
	triggers  map[string]bool
	fetchedAt time.Time
}

// isCommandMessage reports whether a bridged message is a command (no post).
func isCommandMessage(id networkid.MessageID) bool {
	return strings.HasPrefix(string(id), commandMessagePrefix)
}

var errCommandMessage = errors.New("this was a slash command, not a Mattermost post")

// commandTrigger returns the trigger of a "/trigger args" body, lowercased.
func commandTrigger(body string) (string, bool) {
	m := commandWord.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return strings.ToLower(m[1]), true
}

// commandTeam is the team a command runs in: the login's team, else the
// channel's (legacy logins not yet pinned to a team).
func (c *MattermostClient) commandTeam(channelID string) string {
	if t := c.teamScope(); t != "" {
		return t
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if ch := c.channels[channelID]; ch != nil {
		return ch.TeamID
	}
	return ""
}

func (c *MattermostClient) listCommands(teamID string) ([]mattermost.Command, error) {
	if c.listCommandsFn != nil {
		return c.listCommandsFn(teamID)
	}
	return mattermost.ListCommands(c.serverURL(), c.Token, teamID)
}

func (c *MattermostClient) executeCommand(channelID, teamID, rootID, command string) (*mattermost.CommandResponse, error) {
	if c.executeCommandFn != nil {
		return c.executeCommandFn(channelID, teamID, rootID, command)
	}
	return mattermost.ExecuteCommand(c.serverURL(), c.Token, channelID, teamID, rootID, command)
}

// knownCommand reports whether trigger is a command the user can run in team.
func (c *MattermostClient) knownCommand(ctx context.Context, teamID, trigger string) bool {
	now := time.Now()
	c.mu.RLock()
	cached := c.commands[teamID]
	c.mu.RUnlock()
	if cached != nil && now.Sub(cached.fetchedAt) < commandListTTL {
		if cached.triggers[trigger] || now.Sub(cached.fetchedAt) < commandListRefresh {
			return cached.triggers[trigger]
		}
	}
	cmds, err := c.listCommands(teamID)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Could not list Mattermost slash commands")
		return cached != nil && cached.triggers[trigger]
	}
	triggers := make(map[string]bool, len(cmds))
	for _, cmd := range cmds {
		if cmd.Trigger != "" {
			triggers[strings.ToLower(cmd.Trigger)] = true
		}
	}
	c.mu.Lock()
	if c.commands == nil {
		c.commands = make(map[string]*commandCache)
	}
	c.commands[teamID] = &commandCache{triggers: triggers, fetchedAt: now}
	c.mu.Unlock()
	return triggers[trigger]
}

// handleSlashCommand runs content as a Mattermost command when it is one. It
// returns handled=false for ordinary text, which is then posted. body is the
// outbound Markdown, with mention pills as "@username".
func (c *MattermostClient) handleSlashCommand(ctx context.Context, msg *bridgev2.MatrixMessage, channelID, body string) (*bridgev2.MatrixMessageResponse, bool, error) {
	if msg.Content.MsgType != event.MsgText {
		return nil, false, nil
	}
	trigger, ok := commandTrigger(body)
	if !ok {
		return nil, false, nil
	}
	teamID := c.commandTeam(channelID)
	if teamID == "" || !c.knownCommand(ctx, teamID, trigger) {
		return nil, false, nil
	}
	zerolog.Ctx(ctx).Debug().Str("channel", channelID).Str("trigger", trigger).Msg("Running Mattermost slash command")
	rootID := outboundRootID(msg)
	if needsReplyRootLookup(msg) {
		rootID = resolveReplyRoot(rootID, c.fetchReplyTarget(ctx, msg.ReplyTo.ID))
	}
	resp, err := c.executeCommand(channelID, teamID, rootID, strings.TrimSpace(body))
	if err != nil {
		var httpErr *mattermost.HTTPError
		if errors.As(err, &httpErr) && httpErr.Message != "" {
			err = fmt.Errorf("/%s: %s", trigger, httpErr.Message)
		}
		return nil, true, wrapSendErr(err)
	}
	if resp != nil && resp.GotoLocation != "" {
		c.queueNotice(msg.Portal.ID, rootID, fmt.Sprintf("Mattermost opened %s", resp.GotoLocation), true)
	}
	return &bridgev2.MatrixMessageResponse{
		DB: &database.Message{
			ID:       networkid.MessageID(commandMessagePrefix + string(msg.Event.ID)),
			SenderID: networkid.UserID(c.UserID),
		},
	}, true, nil
}

// escapedSlash turns "//text" into "/text", Mattermost's way of posting text
// that starts with a slash.
func escapedSlash(body string) (string, bool) {
	if strings.HasPrefix(body, "//") {
		return body[1:], true
	}
	return body, false
}

const ephemeralNoticeLifetime = 24 * time.Hour

// relayEphemeral shows a post only this user sees (a command's reply) as a
// notice from the bridge, in the post's thread when it has one.
func (c *MattermostClient) relayEphemeral(post *mattermost.Post) {
	if post == nil || post.ChannelID == "" {
		return
	}
	md := strings.TrimSpace(postMarkdown(post))
	if md == "" {
		return
	}
	c.queueNoticeWithExpiry(networkid.PortalID(post.ChannelID), post.RootID, md, true, c.ephemeralNoticeExpiry())
}

// ephemeralNoticeExpiry is the lifetime of a copied private reply, or zero.
func (c *MattermostClient) ephemeralNoticeExpiry() time.Duration {
	if cfg := bridgeConfig(c.bridge()); cfg != nil && cfg.ExpireEphemeralNotices {
		return ephemeralNoticeLifetime
	}
	return 0
}

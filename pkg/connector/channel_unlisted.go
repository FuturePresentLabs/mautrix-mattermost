package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// unlistedPortalIDs returns the channel IDs of this login's bridged portals that
// are absent from the channel listing. Pure so it can be tested without a DB.
func unlistedPortalIDs(portals []*database.Portal, receiver networkid.UserLoginID, listed map[string]bool) []string {
	var out []string
	for _, p := range portals {
		if p == nil || p.MXID == "" || p.Receiver != receiver {
			continue
		}
		// Team spaces are not channels; they must never be probed or deleted.
		if id := string(p.ID); id != "" && !listed[id] && !isTeamPortal(p.ID) {
			out = append(out, id)
		}
	}
	return out
}

// reconcileUnlistedPortals brings the rooms of channels missing from the
// ordinary listing up to date.
//
// That listing omits archived channels, so a channel archived while the bridge
// was offline would otherwise keep a room that never learned it is read-only.
// The lifecycle listing includes archived channels, so the answer here is
// authoritative: an archived channel's room is resynced with its overrides,
// and a channel absent from that listing too is handed to the same deferred
// deletion check a channel_deleted event uses — which probes again over REST
// and fails closed on anything ambiguous.
func (c *MattermostClient) reconcileUnlistedPortals(listed map[string]bool) {
	if c.UserLogin == nil || c.UserLogin.Bridge == nil || c.UserLogin.Bridge.DB == nil {
		return
	}
	log := c.lifecycleLogger()
	portals, err := c.UserLogin.Bridge.DB.Portal.GetAllWithMXID(context.Background())
	if err != nil {
		log.Warn().Err(err).Msg("Could not list portals to reconcile archived channels")
		return
	}
	ids := unlistedPortalIDs(portals, c.receiverID(), listed)
	if len(ids) == 0 {
		return
	}
	// Without this listing every unlisted portal would look deleted, so an
	// error must leave their rooms exactly as they are.
	channels, err := mattermost.GetAllChannelsIncludingArchived(context.Background(), c.serverURL(), c.Token, c.UserID)
	if err != nil {
		log.Debug().Err(err).Int("count", len(ids)).
			Msg("Could not list channels including archived ones; leaving their rooms unchanged")
		return
	}
	for _, id := range ids {
		if ch, ok := channels[id]; ok {
			if ch.DeleteAt > 0 {
				// Archived: the room stays and becomes read-only.
				c.queueChannelResync(ch)
			}
			continue
		}
		// Absent even from the listing that includes archived channels.
		c.handleChannelDeleted(id)
	}
	log.Debug().Int("count", len(ids)).Msg("Reconciled portals missing from the channel listing")
}

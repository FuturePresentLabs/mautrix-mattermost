package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// mediaMsgType maps a MIME type prefix → Matrix message type.
func mediaMsgType(contentType string) event.MessageType {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(contentType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(contentType, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

// threadRelation returns the Matrix relations for a Mattermost post.
//
// Mattermost only has one level of threading: every reply carries root_id
// pointing at the thread root. That maps to a Matrix thread (ThreadRoot). No
// ReplyTo is set: bridgev2 builds the thread fallback reply itself from the
// thread root and previous thread event, which points at the right event even
// when a later reply in the same thread is the fallback target.
func threadRelation(post *mattermost.Post) (replyTo *networkid.MessageOptionalPartID, threadRoot *networkid.MessageID) {
	if post == nil {
		return nil, nil
	}
	replyTo = quotedReplyTarget(post) // set only for quoted replies (quotes.go)
	if post.RootID == "" {
		return replyTo, nil
	}
	root := networkid.MessageID(post.RootID)
	return replyTo, &root
}

// convertPost converts a Mattermost post (text + file attachments) into Matrix message parts.
// intent is used to upload media to the homeserver; it may be nil (e.g. in unit tests without attachments).
func (c *MattermostClient) convertPost(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, post *mattermost.Post) (*bridgev2.ConvertedMessage, error) {
	// Before anything reads the post: a burn-on-read post never goes through the
	// ordinary path (see burn.go).
	if post.IsBurnOnRead() {
		return c.convertBurnOnRead(ctx, post), nil
	}
	var parts []*bridgev2.ConvertedMessagePart
	replyTo, threadRoot := threadRelation(post)

	// Server messages (joins, adds, renames, lifecycle) are notices.
	if post.IsSystem() {
		return systemNotice(post), nil
	}

	// Convert file attachments.
	files := resolveFiles(post)
	parts, err := c.convertAttachmentParts(ctx, intent, roomID, files, false)
	if err != nil {
		return nil, err
	}

	// Convert text body: the message plus any integration attachments.
	quoted := postMarkdown(post)
	if text := withAckPrompt(quoted, c.ackPrompt(post, false)); text != "" {
		if len(parts) > 0 {
			c.embedPostCaption(ctx, intent, roomID, post, parts, text, quoted)
		} else {
			// Text-only message.
			textContent := c.renderPostText(ctx, text)
			textContent.BeeperLinkPreviews = c.linkPreviews(ctx, intent, roomID, post)
			// Quote linked Mattermost posts under the link. Best effort: the
			// message is sent either way.
			c.attachPermalinkQuotes(ctx, &textContent, quoted)
			cm := &bridgev2.ConvertedMessage{
				ReplyTo:    replyTo,
				ThreadRoot: threadRoot,
				Parts: []*bridgev2.ConvertedMessagePart{{
					ID:         networkid.PartID(""),
					Type:       event.EventMessage,
					Content:    &textContent,
					DBMetadata: ackPromptMetadata(c, post, &textContent),
				}},
			}
			return cm, nil
		}
	}

	// Fallback: ensure at least one part is returned.
	if len(parts) == 0 {
		parts = []*bridgev2.ConvertedMessagePart{{
			ID:   networkid.PartID(""),
			Type: event.EventMessage,
			Content: &event.MessageEventContent{
				MsgType: event.MsgText,
				Body:    post.Message,
			},
		}}
	}
	if len(parts) > 0 {
		parts[0].DBMetadata = c.firstPartMetadata(post, parts[0])
	}

	return &bridgev2.ConvertedMessage{
		ReplyTo:    replyTo,
		ThreadRoot: threadRoot,
		Parts:      parts,
	}, nil
}

// resolveFiles returns the FileInfo list from post.Metadata.Files when present,
// otherwise falls back to constructing minimal FileInfo from post.FileIDs.
func resolveFiles(post *mattermost.Post) []*mattermost.FileInfo {
	if post.Metadata != nil && len(post.Metadata.Files) > 0 {
		return post.Metadata.Files
	}
	if len(post.FileIDs) == 0 {
		return nil
	}
	files := make([]*mattermost.FileInfo, 0, len(post.FileIDs))
	for _, id := range post.FileIDs {
		files = append(files, &mattermost.FileInfo{ID: id})
	}
	return files
}

// errEditMediaUnavailable means an edit carries attachments but there is no
// Matrix intent to upload them with. The edit is refused rather than reducing
// the post to text: the original media must stay in place.
var errEditMediaUnavailable = errors.New("media edit requires a Matrix intent")

// uploadPostFile downloads one attachment from Mattermost and uploads it to
// Matrix. Unlike the create path, an error here is returned so an edit can
// fail closed instead of deleting or replacing media it could not rebuild.
func (c *MattermostClient) uploadPostFile(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, fi *mattermost.FileInfo) (*event.MessageEventContent, error) {
	if intent == nil {
		return nil, errEditMediaUnavailable
	}
	fileURL := fmt.Sprintf("%s/api/v4/files/%s", c.serverURL(), fi.ID)
	req, err := http.NewRequestWithContext(ctx, "GET", fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build attachment request for %s: %w", fi.ID, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("download attachment %s: %w", fi.ID, err)
	}
	attData, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read attachment %s: %w", fi.ID, readErr)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download attachment %s: HTTP %d", fi.ID, resp.StatusCode)
	}

	mimeType := fi.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if sniffed := sniffMimeType(attData); sniffed != "" {
		mimeType = sniffed
	}

	filename := fi.Name
	if filename == "" {
		filename = fi.ID
	}

	mxcURL, encFile, uploadErr := intent.UploadMedia(ctx, roomID, attData, filename, mimeType)
	if uploadErr != nil {
		return nil, fmt.Errorf("upload attachment %s: %w", fi.ID, uploadErr)
	}

	content := &event.MessageEventContent{
		MsgType: mediaMsgType(mimeType),
		Body:    filename,
		URL:     mxcURL,
		File:    encFile,
		Info: &event.FileInfo{
			MimeType: mimeType,
			Size:     int(fi.Size),
		},
	}
	if fi.Width > 0 {
		content.Info.Width = fi.Width
	}
	if fi.Height > 0 {
		content.Info.Height = fi.Height
	}
	return content, nil
}

// convertAttachmentParts downloads and uploads the post's files and builds one
// part per attachment. Create ignores a failed attachment, as before; an edit
// passes strict=true so a partial result cannot look like a deletion.
func (c *MattermostClient) convertAttachmentParts(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, files []*mattermost.FileInfo, strict bool) ([]*bridgev2.ConvertedMessagePart, error) {
	parts := make([]*bridgev2.ConvertedMessagePart, 0, len(files))
	for _, fi := range files {
		if intent == nil {
			if strict {
				return nil, errEditMediaUnavailable
			}
			continue // skip uploads when no intent available (unit tests)
		}
		content, err := c.uploadPostFile(ctx, intent, roomID, fi)
		if err != nil {
			if strict {
				return nil, err
			}
			continue
		}
		// bridgev2 synthesizes missing thread-root event IDs with an empty part ID.
		// Use that ID for the first attachment; subsequent parts retain their file ID.
		partID := networkid.PartID("att_" + fi.ID)
		if len(parts) == 0 {
			partID = ""
		}
		parts = append(parts, &bridgev2.ConvertedMessagePart{
			ID:         partID,
			Type:       event.EventMessage,
			Content:    content,
			DBMetadata: &MessageMetadata{MediaFileID: fi.ID},
		})
	}
	return parts, nil
}

// embedPostCaption puts the post text on the first attachment as its caption
// (MSC2530). The original filename stays in FileName so the client can show
// both the caption and the file name.
func (c *MattermostClient) embedPostCaption(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, post *mattermost.Post, parts []*bridgev2.ConvertedMessagePart, text, quoted string) {
	if len(parts) == 0 || text == "" {
		return
	}
	firstContent := parts[0].Content
	firstContent.FileName = firstContent.Body
	rendered := c.renderPostText(ctx, text)
	firstContent.BeeperLinkPreviews = c.linkPreviews(ctx, intent, roomID, post)
	firstContent.Mentions = rendered.Mentions
	firstContent.Body = rendered.Body
	if rendered.FormattedBody != "" {
		firstContent.FormattedBody = rendered.FormattedBody
		firstContent.Format = rendered.Format
	}
	c.attachPermalinkQuotes(ctx, firstContent, quoted)
}

// firstPartMetadata rebuilds the acknowledgement state for the part carrying
// the text, keeping the media identity already assigned to that part.
func (c *MattermostClient) firstPartMetadata(post *mattermost.Post, part *bridgev2.ConvertedMessagePart) *MessageMetadata {
	meta := ackPromptMetadata(c, post, part.Content)
	if old, ok := part.DBMetadata.(*MessageMetadata); ok && old != nil {
		meta.MediaFileID = old.MediaFileID
	}
	return meta
}

// convertPostMediaEditParts builds the parts an edited post has now, failing
// on the first attachment that cannot be downloaded or uploaded. A partial
// result would be indistinguishable from the user deleting that attachment.
func (c *MattermostClient) convertPostMediaEditParts(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, post *mattermost.Post) ([]*bridgev2.ConvertedMessagePart, error) {
	parts, err := c.convertAttachmentParts(ctx, intent, roomID, resolveFiles(post), true)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return parts, nil
	}
	quoted := postMarkdown(post)
	c.embedPostCaption(ctx, intent, roomID, post, parts, withAckPrompt(quoted, c.ackPromptFor(post)), quoted)
	parts[0].DBMetadata = c.firstPartMetadata(post, parts[0])
	return parts, nil
}

// mapEditParts maps freshly converted parts onto the rows already in the
// database. The canonical empty part always keeps the first converted part:
// that is the event replies and thread fallbacks point at, so it is never
// deleted while the post still has content. Later parts match their stable
// att_<fileID> row; unmatched stable rows are deleted and genuinely new
// attachments become additions.
func (c *MattermostClient) mapEditParts(post *mattermost.Post, existing []*database.Message, converted []*bridgev2.ConvertedMessagePart) *bridgev2.ConvertedEdit {
	files := resolveFiles(post)
	byID := make(map[networkid.PartID]*database.Message, len(existing))
	for _, row := range existing {
		if _, ok := byID[row.PartID]; !ok {
			byID[row.PartID] = row
		}
	}
	primary := byID[""]
	used := make(map[*database.Message]bool, len(existing))
	var modified []*bridgev2.ConvertedEditPart
	var added []*bridgev2.ConvertedMessagePart
	for i, part := range converted {
		var match *database.Message
		if i == 0 && primary != nil {
			match = primary
		} else if i < len(files) {
			if row := byID[networkid.PartID("att_"+files[i].ID)]; row != nil && !used[row] {
				match = row
			}
		}
		if match == nil {
			if part.ID == "" && primary != nil && len(files) > 0 {
				// Never add a second canonical part. This is unreachable with
				// a normal conversion, but keeps a malformed caller honest.
				part.ID = networkid.PartID("att_" + files[0].ID)
			}
			added = append(added, part)
			continue
		}
		used[match] = true
		if i == 0 {
			unsafe, revealed := false, false
			if old, ok := match.Metadata.(*MessageMetadata); ok && old != nil {
				unsafe = old.AckPromptUnsafe
				revealed = old.AckPromptRevealedBurn
			}
			meta := &MessageMetadata{
				AckPromptHidden:       c.ackPromptFor(post) == "",
				AckPromptTracked:      true,
				AckPromptUnsafe:       unsafe || len(files) > 0,
				AckPromptRevealedBurn: revealed,
			}
			if i < len(files) {
				meta.MediaFileID = files[i].ID
			}
			match.Metadata = meta
		}
		modified = append(modified, &bridgev2.ConvertedEditPart{
			Part:    match,
			Type:    part.Type,
			Content: part.Content,
		})
	}
	edit := &bridgev2.ConvertedEdit{ModifiedParts: modified}
	for _, row := range existing {
		if used[row] || row.PartID == "" {
			continue
		}
		// Only stable attachment rows are removed. The canonical row is
		// retained even if its file moved, so existing replies still resolve.
		if strings.HasPrefix(string(row.PartID), "att_") {
			edit.DeletedParts = append(edit.DeletedParts, row)
		}
	}
	if len(added) > 0 {
		replyTo, threadRoot := threadRelation(post)
		edit.AddedParts = &bridgev2.ConvertedMessage{
			ReplyTo:    replyTo,
			ThreadRoot: threadRoot,
			Parts:      added,
		}
	}
	return edit
}

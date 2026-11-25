package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// Link previews.
//
// Mattermost unfurls links itself and sends the result with the post
// (metadata.embeds, type "opengraph"). They are passed on as bundled
// previews (com.beeper.linkpreviews, MSC4095), so clients show the title,
// description and image without fetching the page. The image is
// downloaded once (http(s) only, capped in size) and uploaded to Matrix; a
// preview whose image cannot be fetched is sent without one.

const (
	maxPreviews          = 3
	maxPreviewImageBytes = 5 << 20
	previewImageTimeout  = 15 * time.Second
)

// linkPreviews builds the bundled previews for a post's text.
func (c *MattermostClient) linkPreviews(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, post *mattermost.Post) []*event.BeeperLinkPreview {
	if post == nil || post.Metadata == nil {
		return nil
	}
	var out []*event.BeeperLinkPreview
	for _, e := range post.Metadata.Embeds {
		if len(out) == maxPreviews {
			break
		}
		if e == nil || e.Type != "opengraph" || len(e.Data) == 0 {
			continue
		}
		var og mattermost.OpenGraph
		if json.Unmarshal(e.Data, &og) != nil || (og.Title == "" && og.Description == "") {
			continue
		}
		matched := e.URL
		if matched == "" {
			matched = og.URL
		}
		lp := &event.BeeperLinkPreview{
			MatchedURL: matched,
			LinkPreview: event.LinkPreview{
				CanonicalURL: og.URL,
				Title:        og.Title,
				Type:         og.Type,
				Description:  og.Description,
				SiteName:     og.SiteName,
			},
		}
		c.attachPreviewImage(ctx, intent, roomID, post, &og, lp)
		out = append(out, lp)
	}
	return out
}

func (c *MattermostClient) attachPreviewImage(ctx context.Context, intent bridgev2.MatrixAPI, roomID id.RoomID, post *mattermost.Post, og *mattermost.OpenGraph, lp *event.BeeperLinkPreview) {
	if len(og.Images) == 0 || intent == nil {
		return
	}
	img := og.Images[0]
	src := img.SecureURL
	if src == "" {
		src = img.URL
	}
	if src == "" {
		return
	}
	data, mime, err := c.fetchPreviewImage(ctx, src)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Link preview image not fetched; sending the preview without it")
		return
	}
	mxc, enc, err := intent.UploadMedia(ctx, roomID, data, "preview", mime)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Link preview image not uploaded; sending the preview without it")
		return
	}
	lp.ImageURL, lp.ImageEncryption = mxc, enc
	lp.ImageType = mime
	lp.ImageSize = event.IntOrString(len(data))
	width, height := img.Width, img.Height
	if size := post.Metadata.Images[src]; size != nil && (width == 0 || height == 0) {
		width, height = size.Width, size.Height
	}
	if width > 0 && height > 0 {
		lp.ImageWidth, lp.ImageHeight = event.IntOrString(width), event.IntOrString(height)
	}
}

// fetchPreviewImage downloads a preview image: http(s) only, an image type,
// at most maxPreviewImageBytes.
func (c *MattermostClient) fetchPreviewImage(ctx context.Context, src string) ([]byte, string, error) {
	u, err := url.Parse(src)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", fmt.Errorf("unsupported image URL")
	}
	if c.fetchPreviewImageFn != nil {
		return c.fetchPreviewImageFn(src)
	}
	ctx, cancel := context.WithTimeout(ctx, previewImageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPreviewImageBytes+1))
	if err != nil {
		return nil, "", err
	}
	return checkPreviewImage(data, resp.Header.Get("Content-Type"))
}

func checkPreviewImage(data []byte, declared string) ([]byte, string, error) {
	if len(data) > maxPreviewImageBytes {
		return nil, "", fmt.Errorf("image larger than %d bytes", maxPreviewImageBytes)
	}
	mime := sniffMimeType(data)
	if mime == "" {
		mime = strings.TrimSpace(strings.Split(declared, ";")[0])
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", fmt.Errorf("not an image (%q)", mime)
	}
	return data, mime, nil
}

// systemNotice is a server message ("alice joined the channel") as a notice
// from the bridge bot rather than a message from the user it names.
func systemNotice(post *mattermost.Post) *bridgev2.ConvertedMessage {
	_, threadRoot := threadRelation(post)
	return &bridgev2.ConvertedMessage{
		ThreadRoot: threadRoot,
		Parts: []*bridgev2.ConvertedMessagePart{{
			Type:    event.EventMessage,
			Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: post.Message},
		}},
	}
}

// postSender is who a post is bridged as. Server messages come from the
// bridge bot (an empty sender), so they never look like the user wrote them.
func (c *MattermostClient) postSender(post *mattermost.Post) bridgev2.EventSender {
	if post.IsSystem() {
		return bridgev2.EventSender{}
	}
	return c.userSender(post.UserID)
}

// userSender consistently attributes remote activity to the logged-in Matrix
// user when it came from their Mattermost account, rather than their ghost.
func (c *MattermostClient) userSender(userID string) bridgev2.EventSender {
	isFromMe := c.UserID != "" && userID == c.UserID
	sender := bridgev2.EventSender{Sender: networkid.UserID(userID), IsFromMe: isFromMe}
	if isFromMe && c.UserLogin != nil {
		sender.SenderLogin = c.UserLogin.ID
	}
	return sender
}

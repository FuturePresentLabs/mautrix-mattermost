package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type editUploadIntent struct {
	bridgev2.MatrixAPI
	uploads []string
	fail    bool
}

func (e *editUploadIntent) UploadMedia(_ context.Context, _ id.RoomID, _ []byte, name, mime string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	if e.fail {
		return "", nil, errors.New("upload failed")
	}
	e.uploads = append(e.uploads, name)
	return id.ContentURIString("mxc://example.com/" + name), nil, nil
}

func editFileServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "broken") {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(pngBytes)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func imageEditClient(t *testing.T) *MattermostClient {
	t.Helper()
	return newBackfillClient(editFileServer(t).URL)
}

func imageRow(partID networkid.PartID) *database.Message {
	fileID := "image-file"
	if partID != "" {
		fileID = strings.TrimPrefix(string(partID), "att_")
	}
	return &database.Message{
		ID: "p1", PartID: partID, MXID: id.EventID("$original-" + string(partID) + ":example"),
		Metadata: &MessageMetadata{AckPromptUnsafe: true, MediaFileID: fileID},
	}
}

func legacyUnsafeImageRow(partID networkid.PartID) *database.Message {
	return &database.Message{
		ID: "p1", PartID: partID, MXID: id.EventID("$legacy-" + string(partID) + ":example"),
		Metadata: &MessageMetadata{AckPromptUnsafe: true},
	}
}

func TestImageOnlyEditKeepsMedia(t *testing.T) {
	c := imageEditClient(t)
	intent := &editUploadIntent{}
	existing := []*database.Message{imageRow("")}
	post := &mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel", FileIDs: []string{"image-file"}}

	edit, err := c.convertPostEdit(context.Background(), intent, "!room:example", existing, post)
	if err != nil {
		t.Fatalf("convertPostEdit = %v", err)
	}
	if len(edit.ModifiedParts) != 1 {
		t.Fatalf("modified parts = %d, want 1", len(edit.ModifiedParts))
	}
	part := edit.ModifiedParts[0]
	if part.Content.MsgType != event.MsgImage || part.Content.URL == "" || part.Content.Body == "" {
		t.Fatalf("content = %+v, want the image preserved", part.Content)
	}
	if part.Part != existing[0] || part.Part.MXID != "$original-:example" {
		t.Fatalf("edit part = %+v, want the original row and MXID", part.Part)
	}
	if len(edit.DeletedParts) != 0 || edit.AddedParts != nil {
		t.Fatalf("edit deleted/added parts: %+v / %+v", edit.DeletedParts, edit.AddedParts)
	}
	if len(intent.uploads) != 1 {
		t.Fatalf("uploads = %v, want one", intent.uploads)
	}
}

func TestImageCaptionEditKeepsMediaAndUsesFilenameConvention(t *testing.T) {
	c := imageEditClient(t)
	intent := &editUploadIntent{}
	existing := []*database.Message{imageRow("")}
	post := &mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel",
		FileIDs: []string{"image-file"}, Message: "a new caption"}

	edit, err := c.convertPostEdit(context.Background(), intent, "!room:example", existing, post)
	if err != nil {
		t.Fatalf("convertPostEdit = %v", err)
	}
	if len(edit.ModifiedParts) != 1 {
		t.Fatalf("modified parts = %d, want 1", len(edit.ModifiedParts))
	}
	content := edit.ModifiedParts[0].Content
	if content.MsgType != event.MsgImage || content.Body != "a new caption" || content.FileName != "image-file" {
		t.Fatalf("content = %+v, want image with caption and original filename", content)
	}
}

func TestMultiAttachmentEditMapsByPartID(t *testing.T) {
	c := imageEditClient(t)
	intent := &editUploadIntent{}
	existing := []*database.Message{imageRow(""), imageRow("att_second")}
	post := &mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel",
		FileIDs: []string{"first", "second", "third"}}

	edit, err := c.convertPostEdit(context.Background(), intent, "!room:example", existing, post)
	if err != nil {
		t.Fatalf("convertPostEdit = %v", err)
	}
	if len(edit.ModifiedParts) != 2 || len(edit.DeletedParts) != 0 {
		t.Fatalf("modified/deleted = %d/%d, want 2/0", len(edit.ModifiedParts), len(edit.DeletedParts))
	}
	if edit.ModifiedParts[1].Part != existing[1] {
		t.Fatalf("second part = %+v, want att_second row", edit.ModifiedParts[1].Part)
	}
	if edit.AddedParts == nil || len(edit.AddedParts.Parts) != 1 || edit.AddedParts.Parts[0].ID != "att_third" {
		t.Fatalf("added = %+v, want the stable att_third part", edit.AddedParts)
	}

	// Removing the first attachment keeps the canonical event: it is what
	// existing replies point at. The surviving file is written there and the
	// now-orphaned stable row is deleted.
	edit, err = c.convertPostEdit(context.Background(), intent, "!room:example", existing,
		&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel", FileIDs: []string{"second"}})
	if err != nil {
		t.Fatalf("remove first = %v", err)
	}
	if len(edit.ModifiedParts) != 1 || edit.ModifiedParts[0].Part != existing[0] {
		t.Fatalf("modified = %+v, want the canonical first row", edit.ModifiedParts)
	}
	if edit.ModifiedParts[0].Part.MXID != "$original-:example" {
		t.Fatalf("canonical MXID = %s, want the original reply target", edit.ModifiedParts[0].Part.MXID)
	}
	if len(edit.DeletedParts) != 1 || edit.DeletedParts[0] != existing[1] {
		t.Fatalf("deleted = %+v, want the orphaned att_second row", edit.DeletedParts)
	}
	if edit.AddedParts != nil {
		t.Fatalf("added = %+v, want none", edit.AddedParts)
	}
}

func TestReorderEditKeepsCanonicalAndAddsNoEmptyPart(t *testing.T) {
	c := imageEditClient(t)
	intent := &editUploadIntent{}
	existing := []*database.Message{imageRow(""), imageRow("att_second")}
	edit, err := c.convertPostEdit(context.Background(), intent, "!room:example", existing,
		&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel", FileIDs: []string{"second", "third"}})
	if err != nil {
		t.Fatalf("convertPostEdit = %v", err)
	}
	if len(edit.ModifiedParts) != 1 || edit.ModifiedParts[0].Part != existing[0] {
		t.Fatalf("modified = %+v, want only the canonical row", edit.ModifiedParts)
	}
	if len(edit.DeletedParts) != 1 || edit.DeletedParts[0] != existing[1] {
		t.Fatalf("deleted = %+v, want att_second", edit.DeletedParts)
	}
	if edit.AddedParts == nil || len(edit.AddedParts.Parts) != 1 {
		t.Fatalf("added = %+v, want one new attachment", edit.AddedParts)
	}
	if id := edit.AddedParts.Parts[0].ID; id != "att_third" {
		t.Fatalf("added part ID = %q, want att_third (never a duplicate empty ID)", id)
	}
}

func TestFailedAttachmentEditFailsClosed(t *testing.T) {
	c := imageEditClient(t)
	existing := []*database.Message{imageRow("")}

	for name, tc := range map[string]struct {
		intent bridgev2.MatrixAPI
		fileID string
	}{
		"nil intent":    {nil, "image-file"},
		"download fail": {&editUploadIntent{}, "broken-file"},
		"upload fail":   {&editUploadIntent{fail: true}, "image-file"},
	} {
		t.Run(name, func(t *testing.T) {
			edit, err := c.convertPostEdit(context.Background(), tc.intent, "!room:example", existing,
				&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel", FileIDs: []string{tc.fileID}})
			if err == nil || edit != nil {
				t.Fatalf("convertPostEdit = %+v, %v; want a closed failure", edit, err)
			}
			if existing[0].Metadata.(*MessageMetadata).AckPromptHidden {
				t.Fatal("a failed edit must not mutate the stored row")
			}
		})
	}
}

func TestMetadataOnlyEditDoesNotEraseMedia(t *testing.T) {
	c := &MattermostClient{UserID: "reader"}
	existing := []*database.Message{imageRow("")}
	post := &mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel"}

	edit, err := c.convertPostEdit(context.Background(), nil, "", existing, post)
	if !errors.Is(err, bridgev2.ErrIgnoringRemoteEvent) || edit != nil {
		t.Fatalf("convertPostEdit = %+v, %v; want ignored", edit, err)
	}
}

func TestTextToImageToTextTransitions(t *testing.T) {
	c := imageEditClient(t)
	intent := &editUploadIntent{}
	existing := []*database.Message{{ID: "p1", PartID: "", MXID: "$text:example"}}

	edit, err := c.convertPostEdit(context.Background(), intent, "!room:example", existing,
		&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel", FileIDs: []string{"image-file"}})
	if err != nil || len(edit.ModifiedParts) != 1 || edit.ModifiedParts[0].Content.MsgType != event.MsgImage {
		t.Fatalf("text -> image = %+v, %v", edit, err)
	}

	// Replacing the only image with text edits the same event; nothing is
	// deleted because the image was the primary part being rewritten.
	edit, err = c.convertPostEdit(context.Background(), nil, "", existing,
		&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel",
			FileIDs: []string{}, Message: "back to text"})
	if err != nil {
		t.Fatalf("image -> text = %v", err)
	}
	if len(edit.ModifiedParts) != 1 || edit.ModifiedParts[0].Content.MsgType != event.MsgText ||
		edit.ModifiedParts[0].Content.Body != "back to text" {
		t.Fatalf("modified = %+v, want text", edit.ModifiedParts)
	}
	if len(edit.DeletedParts) != 0 {
		t.Fatalf("deleted = %+v, want none for a single primary part", edit.DeletedParts)
	}
}

func TestTextToImageToTextDeletesExtraMediaParts(t *testing.T) {
	c := imageEditClient(t)
	existing := []*database.Message{imageRow(""), imageRow("att_second")}

	edit, err := c.convertPostEdit(context.Background(), nil, "", existing,
		&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel",
			FileIDs: []string{}, Message: "back to text"})
	if err != nil {
		t.Fatalf("convertPostEdit = %v", err)
	}
	if len(edit.ModifiedParts) != 1 || edit.ModifiedParts[0].Part != existing[0] {
		t.Fatalf("modified = %+v, want the primary row", edit.ModifiedParts)
	}
	if len(edit.DeletedParts) != 1 || edit.DeletedParts[0] != existing[1] {
		t.Fatalf("deleted = %+v, want the extra attachment row", edit.DeletedParts)
	}
	if meta, _ := existing[0].Metadata.(*MessageMetadata); meta == nil || meta.MediaFileID != "" {
		t.Fatalf("primary metadata = %#v, want media identity cleared", existing[0].Metadata)
	}
}

func TestPartialCaptionSnapshotOnKnownMediaIsIgnored(t *testing.T) {
	c := &MattermostClient{UserID: "reader"}
	for name, existing := range map[string][]*database.Message{
		"media file id": {imageRow("")},
		"legacy unsafe": {legacyUnsafeImageRow("")},
	} {
		t.Run(name, func(t *testing.T) {
			post := &mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel", Message: "changed caption"}
			edit, err := c.convertPostEdit(context.Background(), nil, "", existing, post)
			if !errors.Is(err, bridgev2.ErrIgnoringRemoteEvent) || edit != nil {
				t.Fatalf("convertPostEdit = %+v, %v; want the partial snapshot ignored", edit, err)
			}
		})
	}
}

func TestInitialMediaConversionStoresFileIdentity(t *testing.T) {
	c := imageEditClient(t)
	intent := &editUploadIntent{}
	result, err := c.convertPost(context.Background(), intent, "!room:example",
		&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel", FileIDs: []string{"first", "second"}})
	if err != nil {
		t.Fatalf("convertPost = %v", err)
	}
	if len(result.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(result.Parts))
	}
	for i, want := range []string{"first", "second"} {
		meta, ok := result.Parts[i].DBMetadata.(*MessageMetadata)
		if !ok || meta == nil || meta.MediaFileID != want {
			t.Fatalf("part %d metadata = %#v, want MediaFileID %q", i, result.Parts[i].DBMetadata, want)
		}
	}
}

func TestMediaEditPartsWithNoFilesDoesNotPanic(t *testing.T) {
	c := imageEditClient(t)
	parts, err := c.convertPostMediaEditParts(context.Background(), &editUploadIntent{}, "!room:example",
		&mattermost.Post{ID: "p1", UserID: "alice", ChannelID: "channel"})
	if err != nil || len(parts) != 0 {
		t.Fatalf("convertPostMediaEditParts = %+v, %v; want no parts", parts, err)
	}
}

func TestOrdinaryTextEditStillWorks(t *testing.T) {
	c := &MattermostClient{UserID: "me"}
	existing := []*database.Message{{ID: "p1", PartID: "", MXID: "$text:example", Metadata: &MessageMetadata{AckPromptTracked: true}}}
	edit, err := c.convertPostEdit(context.Background(), nil, "", existing,
		&mattermost.Post{ID: "p1", UserID: "me", ChannelID: "channel", Message: "edited"})
	if err != nil || len(edit.ModifiedParts) != 1 || edit.ModifiedParts[0].Content.Body != "edited" {
		t.Fatalf("convertPostEdit = %+v, %v", edit, err)
	}
}

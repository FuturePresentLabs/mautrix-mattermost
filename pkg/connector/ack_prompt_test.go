package connector

import (
	"context"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/bostrot/mautrix-mattermost/pkg/mattermost"
)

// The prompt is what tells a Beeper reader a post is waiting for them to
// acknowledge it; without it the gesture cannot be discovered.
const (
	promptLine     = "✅ Any reaction acknowledges."
	burnPromptLine = "✅ Acknowledgement requested. After revealing it, react again to acknowledge."
)

func askingPostWithText(author, text string) *mattermost.Post {
	p := askingPost(author)
	p.Message = text
	return p
}

func bodyOf(t *testing.T, c *MattermostClient, post *mattermost.Post) string {
	t.Helper()
	cm, err := c.convertPost(context.Background(), nil, "", post)
	if err != nil || cm == nil || len(cm.Parts) == 0 {
		t.Fatalf("convertPost = %+v, %v", cm, err)
	}
	return cm.Parts[0].Content.Body
}

func TestAPostAskingForAcknowledgementCarriesAPrompt(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	body := bodyOf(t, c, askingPostWithText(otherID, "Please confirm the schedule"))
	if !strings.HasPrefix(body, "Please confirm the schedule") || !strings.HasSuffix(strings.TrimSpace(body), promptLine) {
		t.Errorf("body = %q, want the message followed by %q", body, promptLine)
	}
}

func TestNoPromptWhenThereIsNothingToAcknowledge(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	acked := askingPostWithText(otherID, "hello")
	acked.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: meID, PostID: "p1", AcknowledgedAt: 5}}
	no := false
	for name, post := range map[string]*mattermost.Post{
		"not requested":            {ID: "p1", UserID: otherID, ChannelID: "c1", Message: "hello"},
		"requested explicitly off": {ID: "p1", UserID: otherID, ChannelID: "c1", Message: "hello", Metadata: &mattermost.PostMetadata{Priority: &mattermost.PostPriority{RequestedAck: &no}}},
		"my own request":           askingPostWithText(meID, "hello"),
		"already acknowledged":     acked,
	} {
		if body := bodyOf(t, c, post); strings.Contains(body, "Acknowledgement requested") {
			t.Errorf("%s: body = %q; there is nothing to prompt for", name, body)
		}
	}
	// Someone else's acknowledgement does not satisfy this reader.
	other := askingPostWithText(otherID, "hello")
	other.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: "carol", PostID: "p1", AcknowledgedAt: 5}}
	if body := bodyOf(t, c, other); !strings.Contains(body, promptLine) {
		t.Errorf("body = %q; someone else's acknowledgement must not hide the prompt", body)
	}
}

// A post with nothing but attachments still tells the reader what is asked.
func TestAPostWithNoTextStillGetsThePrompt(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	body := bodyOf(t, c, askingPostWithText(otherID, ""))
	if strings.TrimSpace(body) != promptLine {
		t.Errorf("body = %q, want just the prompt", body)
	}
}

// An edit rebuilds the text from the post as it is then, so it uses the same
// builder: the prompt follows whatever the acknowledgement state is at that
// moment. (Acknowledging itself arrives as its own event, not as an edit.)
func TestThePromptFollowsTheAcknowledgementThroughEdits(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	edit := func(post *mattermost.Post) string {
		ce, err := c.convertPostEdit(context.Background(), nil, "", []*database.Message{{ID: "p1"}}, post)
		if err != nil || len(ce.ModifiedParts) != 1 {
			t.Fatalf("convertPostEdit = %+v, %v", ce, err)
		}
		return ce.ModifiedParts[0].Content.Body
	}
	post := askingPostWithText(otherID, "hello")
	if body := edit(post); !strings.Contains(body, promptLine) {
		t.Errorf("before acknowledging: %q, want the prompt", body)
	}
	post.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: meID, PostID: "p1", AcknowledgedAt: 5}}
	if body := edit(post); strings.Contains(body, "Acknowledgement requested") || !strings.Contains(body, "hello") {
		t.Errorf("after acknowledging: %q, want the text without the prompt", body)
	}
	post.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: meID, PostID: "p1", AcknowledgedAt: 0}}
	if body := edit(post); !strings.Contains(body, promptLine) {
		t.Errorf("after withdrawing: %q, want the prompt back", body)
	}
}

func TestTheBurnPlaceholderCarriesThePromptToo(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	yes := true
	asking := burnPost(otherID, "", true)
	asking.Metadata = &mattermost.PostMetadata{Priority: &mattermost.PostPriority{RequestedAck: &yes}}
	body := bodyOf(t, c, asking)
	if !strings.Contains(body, "Any reaction reveals it") || !strings.HasSuffix(strings.TrimSpace(body), burnPromptLine) {
		t.Errorf("placeholder = %q, want the burn text then %q", body, burnPromptLine)
	}
	if body := bodyOf(t, c, burnPost(otherID, "", true)); strings.Contains(body, "Acknowledgement requested") {
		t.Errorf("placeholder = %q; nothing was requested", body)
	}
}

func TestRevealedTextCarriesThePromptWhenOneIsWaiting(t *testing.T) {
	yes := true
	for name, tc := range map[string]struct {
		acked bool
		want  bool
	}{"waiting": {false, true}, "already acknowledged": {true, false}} {
		revealed := burnPost(otherID, secretText, true)
		revealed.Metadata = &mattermost.PostMetadata{ExpireAt: burnExpiry.UnixMilli(), Priority: &mattermost.PostPriority{RequestedAck: &yes}}
		if tc.acked {
			revealed.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: meID, PostID: "p1", AcknowledgedAt: 5}}
		}
		bs := &burnServer{post: burnPost(otherID, "", true), revealed: revealed}
		c := &MattermostClient{UserID: meID, Token: "tok", baseURL: bs.start(t).URL}
		got, err := c.revealBurnOnRead(context.Background(), "p1")
		if err != nil {
			t.Fatal(err)
		}
		body := got.Content.Body
		if !strings.Contains(body, secretText) || strings.Contains(body, promptLine) != tc.want {
			t.Errorf("%s: revealed body = %q, want the text, prompt=%t", name, body, tc.want)
		}
	}
}

// The prompt belongs to the message in Beeper, not to the text other features
// quote or relay.
func TestThePromptIsNotPartOfThePostText(t *testing.T) {
	if got := postMarkdown(askingPostWithText(otherID, "hello")); strings.Contains(got, "Acknowledgement requested") {
		t.Errorf("postMarkdown = %q; quotes and private replies must not carry the prompt", got)
	}
}

// Conversion records whether the visible body carries the instruction, and
// whether that fact is trustworthy. It records nothing else.
func TestAckPromptStateIsRecordedByConversion(t *testing.T) {
	c := &MattermostClient{UserID: meID}
	metaOf := func(cm *bridgev2.ConvertedMessage) *MessageMetadata {
		t.Helper()
		if len(cm.Parts) == 0 {
			t.Fatal("no parts")
		}
		meta, ok := cm.Parts[0].DBMetadata.(*MessageMetadata)
		if !ok || meta == nil {
			t.Fatalf("metadata = %#v", cm.Parts[0].DBMetadata)
		}
		return meta
	}
	shown, err := c.convertPost(context.Background(), nil, "", askingPostWithText(otherID, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if meta := metaOf(shown); !meta.AckPromptTracked || meta.AckPromptHidden {
		t.Errorf("asking post metadata = %+v, want tracked and shown", meta)
	}
	acked := askingPostWithText(otherID, "hello")
	acked.Metadata.Acknowledgements = []*mattermost.Acknowledgement{{UserID: meID, PostID: "p1", AcknowledgedAt: 5}}
	hidden, err := c.convertPost(context.Background(), nil, "", acked)
	if err != nil {
		t.Fatal(err)
	}
	if meta := metaOf(hidden); !meta.AckPromptTracked || !meta.AckPromptHidden {
		t.Errorf("acknowledged post metadata = %+v, want tracked and hidden", meta)
	}
	if meta := metaOf(c.convertBurnOnRead(context.Background(), burnPost(otherID, "", true))); !meta.AckPromptTracked || !meta.AckPromptHidden {
		t.Errorf("burn post without a request metadata = %+v, want tracked and hidden", meta)
	}
	withFiles := askingPostWithText(otherID, "hello")
	withFiles.FileIDs = []string{"file1"}
	fileConverted, err := c.convertPost(context.Background(), nil, "", withFiles)
	if err != nil {
		t.Fatal(err)
	}
	if meta := metaOf(fileConverted); !meta.AckPromptUnsafe {
		t.Errorf("attachment metadata = %+v, want unsafe", meta)
	}
	preview := &event.MessageEventContent{BeeperLinkPreviews: []*event.BeeperLinkPreview{{}}}
	if meta := ackPromptMetadata(c, askingPostWithText(otherID, "hello"), preview); !meta.AckPromptUnsafe {
		t.Errorf("preview metadata = %+v, want unsafe", meta)
	}
	if meta := ackPromptMetadata(c, askingPostWithText(otherID, "hello"), &event.MessageEventContent{}); meta.AckPromptUnsafe {
		t.Errorf("plain text metadata = %+v, want safe", meta)
	}

	// An edit merges the state into the row it edits rather than dropping it.
	edit, err := c.convertPostEdit(context.Background(), nil, "", []*database.Message{{ID: "p1", Metadata: &MessageMetadata{}}}, acked)
	if err != nil || len(edit.ModifiedParts) != 1 {
		t.Fatalf("convertPostEdit = %+v, %v", edit, err)
	}
	got, ok := edit.ModifiedParts[0].Part.Metadata.(*MessageMetadata)
	if !ok || !got.AckPromptTracked || !got.AckPromptHidden {
		t.Fatalf("edit metadata = %#v, want tracked and hidden", edit.ModifiedParts[0].Part.Metadata)
	}
}

// The builder's exact output: the renderer trims leading blank lines, so the
// end-to-end tests cannot see a stray separator in front of a lone prompt.
func TestWithAckPromptLayout(t *testing.T) {
	for name, tc := range map[string]struct{ text, prompt, want string }{
		"no prompt":         {"hello", "", "hello"},
		"text and prompt":   {"hello", promptLine, "hello\n\n" + promptLine},
		"empty text":        {"", promptLine, promptLine},
		"blank text":        {" \n ", promptLine, promptLine},
		"nothing at all":    {"", "", ""},
		"no prompt no text": {"  ", "", "  "},
	} {
		if got := withAckPrompt(tc.text, tc.prompt); got != tc.want {
			t.Errorf("%s: withAckPrompt(%q, prompt) = %q, want %q", name, tc.text, got, tc.want)
		}
	}
}

package slack

import (
	"context"
	"fmt"

	slackgo "github.com/slack-go/slack"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/release"
)

// Notifier posts and updates the approval message of a release.
type Notifier interface {
	// Required is the number of approvals a release needs.
	Required() int
	// Post announces rec and returns where the message was posted.
	Post(ctx context.Context, plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord, rel *release.Release) (channelID, ts string, err error)
	// Update re-renders the message referenced by rec.Slack.
	Update(ctx context.Context, plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord, rel *release.Release) error
}

// Client is the Slack Web API side of the integration.
type Client struct {
	api *slackgo.Client
	cfg Config
}

// NewClient creates a client from a validated Config.
func NewClient(cfg Config) *Client {
	return &Client{api: slackgo.New(cfg.BotToken, slackgo.OptionAppLevelToken(cfg.AppToken)), cfg: cfg}
}

func (c *Client) Required() int { return c.cfg.Required }

func fallbackText(rec *patchv1alpha1.ApprovalRecord) string {
	return fmt.Sprintf("Talos %s is available: %s", rec.Version, statusLine(rec))
}

func (c *Client) Post(ctx context.Context, plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord, rel *release.Release) (string, string, error) {
	return c.api.PostMessageContext(ctx, c.cfg.ChannelID,
		slackgo.MsgOptionBlocks(Blocks(plan, rec, rel)...), slackgo.MsgOptionText(fallbackText(rec), false))
}

func (c *Client) Update(ctx context.Context, plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord, rel *release.Release) error {
	_, _, _, err := c.api.UpdateMessageContext(ctx, rec.Slack.ChannelID, rec.Slack.MessageTS,
		slackgo.MsgOptionBlocks(Blocks(plan, rec, rel)...), slackgo.MsgOptionText(fallbackText(rec), false))
	return err
}

// MarkStale replaces a message that no longer matches the current approval, removing its buttons.
func (c *Client) MarkStale(ctx context.Context, channelID, ts string) error {
	const text = "This release approval is no longer active."
	section := slackgo.NewSectionBlock(slackgo.NewTextBlockObject(slackgo.MarkdownType, text, false, false), nil, nil)
	_, _, _, err := c.api.UpdateMessageContext(ctx, channelID, ts, slackgo.MsgOptionBlocks(section), slackgo.MsgOptionText(text, false))
	return err
}

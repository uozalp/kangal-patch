package slack

import (
	"context"
	"fmt"

	slackgo "github.com/slack-go/slack"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/release"
	"github.com/uozalp/kangal-patch/internal/rollout"
)

// Notifier posts and updates the approval message of a release.
type Notifier interface {
	// Required is the number of approvals a release needs.
	Required() int
	// Post announces rec and returns where the message was posted.
	Post(ctx context.Context, plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord, rel *release.Release) (channelID, ts string, err error)
	// Update re-renders the message referenced by rec.Slack.
	Update(ctx context.Context, plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord, rel *release.Release) error
	// PostRollout posts the rollout status as a reply in the thread and returns its timestamp.
	PostRollout(ctx context.Context, channelID, threadTS string, snap rollout.Snapshot) (ts string, err error)
	// UpdateRollout re-renders a status message posted by PostRollout.
	UpdateRollout(ctx context.Context, channelID, ts string, snap rollout.Snapshot) error
	// PostFailure posts a reply in the thread about the nodes that failed, asking for manual action
	// if the rollout halted.
	PostFailure(ctx context.Context, channelID, threadTS string, snap rollout.Snapshot, nodes []string) error
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

func (c *Client) PostRollout(ctx context.Context, channelID, threadTS string, snap rollout.Snapshot) (string, error) {
	_, ts, err := c.api.PostMessageContext(ctx, channelID, slackgo.MsgOptionTS(threadTS),
		slackgo.MsgOptionBlocks(RolloutBlocks(snap)...), slackgo.MsgOptionText(rolloutFallback(snap), false))
	return ts, err
}

func (c *Client) UpdateRollout(ctx context.Context, channelID, ts string, snap rollout.Snapshot) error {
	_, _, _, err := c.api.UpdateMessageContext(ctx, channelID, ts,
		slackgo.MsgOptionBlocks(RolloutBlocks(snap)...), slackgo.MsgOptionText(rolloutFallback(snap), false))
	return err
}

func (c *Client) PostFailure(ctx context.Context, channelID, threadTS string, snap rollout.Snapshot, nodes []string) error {
	_, _, err := c.api.PostMessageContext(ctx, channelID, slackgo.MsgOptionTS(threadTS), slackgo.MsgOptionText(failureText(snap, nodes), false))
	return err
}

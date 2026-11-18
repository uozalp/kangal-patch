package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	slackgo "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

const reconnectDelay = 30 * time.Second

// Listener receives button clicks over Socket Mode and records them in the PatchPlan status. It does
// not update the Slack message; the reconciler does that when the status changes.
type Listener struct {
	Config Config
	Slack  *Client
	// Reader must bypass the cache so a decision is made on the latest status.
	Reader client.Reader
	Writer client.StatusClient
}

// NeedLeaderElection is false so every replica holds a connection and no click is lost during
// failover; the optimistic lock in UpdateApproval keeps concurrent writers correct.
func (l *Listener) NeedLeaderElection() bool { return false }

// Start runs until ctx is done. A Slack failure is retried, never fatal to the operator.
func (l *Listener) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("slack")
	for ctx.Err() == nil {
		if err := l.run(ctx); err != nil && ctx.Err() == nil {
			logger.Error(err, "socket mode connection ended, retrying", "in", reconnectDelay.String())
		}
		select {
		case <-ctx.Done():
		case <-time.After(reconnectDelay):
		}
	}
	return nil
}

func (l *Listener) run(ctx context.Context) error {
	sm := socketmode.New(l.Slack.api)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		logger := log.FromContext(ctx).WithName("slack")
		for {
			select {
			case <-runCtx.Done():
				return
			case evt := <-sm.Events:
				switch evt.Type {
				case socketmode.EventTypeConnected:
					logger.Info("connected to Slack")
				case socketmode.EventTypeConnectionError:
					logger.Info("Slack connection error", "event", fmt.Sprint(evt.Data))
				}
				if evt.Type != socketmode.EventTypeInteractive || evt.Request == nil {
					continue
				}
				if err := sm.Ack(*evt.Request); err != nil {
					logger.Info("Slack ack failed", "reason", err.Error())
				}
				cb, ok := evt.Data.(slackgo.InteractionCallback)
				if !ok {
					continue
				}
				if err := l.Handle(runCtx, cb); err != nil {
					logger.Info("ignored interaction", "reason", err.Error(), "user", cb.User.ID)
					continue
				}
				logger.Info("handled interaction", "user", cb.User.ID)
			}
		}
	}()
	return sm.RunContext(runCtx)
}

// Handle applies one button click. Only the user ID, approval ID and channel of the click are
// compared with the state in Kubernetes; nothing else in the payload is trusted.
func (l *Listener) Handle(ctx context.Context, cb slackgo.InteractionCallback) error {
	if cb.Type != slackgo.InteractionTypeBlockActions || len(cb.ActionCallback.BlockActions) == 0 {
		return nil
	}
	act := cb.ActionCallback.BlockActions[0]
	var action Action
	switch act.ActionID {
	case ApproveActionID:
		action = Approve
	case RejectActionID:
		action = Reject
	default:
		return nil
	}

	planName, id, version, ok := ParseButtonValue(act.Value)
	if !ok {
		return fmt.Errorf("malformed button value")
	}
	if !slices.Contains(l.Config.Approvers, cb.User.ID) {
		return ErrNotApprover
	}
	channelID := cb.Container.ChannelID
	if channelID == "" {
		channelID = cb.Channel.ID
	}

	var stale bool
	plan := &patchv1alpha1.PatchPlan{ObjectMeta: metav1.ObjectMeta{Name: planName}}
	err := UpdateApproval(ctx, l.Reader, l.Writer, plan, func(st *patchv1alpha1.ApprovalStatus) error {
		cur := st.Current
		stale = cur == nil || cur.ID != id || cur.Version != version || cur.Slack == nil || cur.Slack.ChannelID != channelID
		if stale {
			return nil
		}
		_, err := Apply(cur, l.Config.Approvers, cb.User.ID, action, time.Now())
		return err
	})
	switch {
	case apierrors.IsNotFound(err):
		stale = true
	case err != nil:
		return err
	}
	if !stale {
		return nil
	}
	if err := l.Slack.MarkStale(ctx, channelID, cb.Container.MessageTs); err != nil {
		return errors.Join(errors.New("stale approval"), err)
	}
	return errors.New("stale approval")
}

package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	slackgo "github.com/slack-go/slack"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/rollout"
)

const (
	// maxTableRunes keeps the table inside the 3000 character limit of a Slack section.
	maxTableRunes   = 2800
	maxFailureLines = 5
	maxFailureRunes = 200
)

// rolloutText is the rendering of a rollout snapshot.
type rolloutText struct {
	header, summary, table, failures string
}

func (t rolloutText) String() string {
	return strings.Join([]string{t.header, t.summary, t.table, t.failures}, "\n")
}

func renderRollout(s rollout.Snapshot) rolloutText {
	return rolloutText{
		header:   "Rollout of Talos " + s.Version,
		summary:  rolloutSummary(s),
		table:    nodeTable(s.Nodes),
		failures: failureLines(s.Nodes),
	}
}

// RolloutKey identifies what the status message shows for s; it changes whenever the message would.
func RolloutKey(s rollout.Snapshot) string {
	sum := sha256.Sum256([]byte(renderRollout(s).String()))
	return hex.EncodeToString(sum[:8])
}

// RolloutBlocks renders the status message of a rollout.
func RolloutBlocks(s rollout.Snapshot) []slackgo.Block {
	t := renderRollout(s)
	text := func(md string) *slackgo.TextBlockObject {
		return slackgo.NewTextBlockObject(slackgo.MarkdownType, md, false, false)
	}
	blocks := []slackgo.Block{
		slackgo.NewHeaderBlock(slackgo.NewTextBlockObject(slackgo.PlainTextType, t.header, false, false)),
		slackgo.NewSectionBlock(text(t.summary), nil, nil),
	}
	if t.table != "" {
		blocks = append(blocks, slackgo.NewSectionBlock(text(t.table), nil, nil))
	}
	if t.failures != "" {
		blocks = append(blocks, slackgo.NewSectionBlock(text(t.failures), nil, nil))
	}
	return blocks
}

func rolloutFallback(s rollout.Snapshot) string {
	return fmt.Sprintf("Rollout of Talos %s: %s", s.Version, strings.ReplaceAll(rolloutSummary(s), "*", ""))
}

func failureText(s rollout.Snapshot, nodes []string) string {
	var lines []string
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if !slices.Contains(nodes, n.Name) {
			continue
		}
		line := "*Node failed:* " + escapeMrkdwn.Replace(n.Name)
		if n.Message != "" {
			line += ": " + escapeMrkdwn.Replace(truncate(n.Message, maxFailureRunes))
		}
		lines = append(lines, line)
	}
	if s.Halted {
		lines = append(lines, fmt.Sprintf("*Halted, manual action needed* on plan %s.", s.Plan))
		if s.Message != "" {
			lines = append(lines, escapeMrkdwn.Replace(truncate(s.Message, maxFailureRunes)))
		}
	}
	return strings.Join(lines, "\n")
}

func rolloutSummary(s rollout.Snapshot) string {
	done, failed := s.Count(patchv1alpha1.PatchJobPhaseCompleted), s.Count(patchv1alpha1.PatchJobPhaseFailed)
	progress := fmt.Sprintf("%d/%d nodes patched", done, len(s.Nodes))
	if failed > 0 {
		progress += fmt.Sprintf(", %d failed", failed)
	}
	if s.Duration > 0 {
		progress += " in " + formatDuration(s.Duration)
	}

	var line string
	switch s.Phase {
	case patchv1alpha1.PatchPhaseCompleted:
		line = "*Completed:* " + progress
	case patchv1alpha1.PatchPhaseFailed:
		line = "*Failed:* " + progress
	case patchv1alpha1.PatchPhaseCancelled:
		line = "*Cancelled:* " + progress
	case patchv1alpha1.PatchPhasePaused:
		line = "*Paused:* " + progress
	default:
		line = "*In progress:* " + progress
	}
	switch s.Phase {
	case patchv1alpha1.PatchPhaseFailed, patchv1alpha1.PatchPhasePaused, patchv1alpha1.PatchPhaseCancelled:
		if s.Message != "" {
			line += "\n" + escapeMrkdwn.Replace(truncate(s.Message, maxFailureRunes))
		}
	}
	return line
}

// nodeTable renders the nodes as a monospace table, leaving out the least interesting nodes when
// the table would not fit in a message.
func nodeTable(nodes []rollout.Node) string {
	if len(nodes) == 0 {
		return ""
	}
	hide := []patchv1alpha1.PatchJobPhase{
		patchv1alpha1.PatchJobPhaseCompleted,
		patchv1alpha1.PatchJobPhasePending,
		patchv1alpha1.PatchJobPhaseFailed,
	}
	for n := 0; n <= len(hide); n++ {
		table := renderTable(nodes, hide[:n])
		if len([]rune(table)) <= maxTableRunes {
			return table
		}
	}
	return renderTable(nodes, hide)
}

func renderTable(nodes []rollout.Node, hide []patchv1alpha1.PatchJobPhase) string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NODE\tGROUP\tPHASE\tTIME")
	hidden := 0
	for i := range nodes {
		n := &nodes[i]
		if slices.Contains(hide, n.Phase) {
			hidden++
			continue
		}
		// Job times have second precision, so a node that needed no work took 0s.
		took := ""
		if n.Phase.IsTerminal() {
			took = formatDuration(n.Duration)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n.Name, n.Group, n.Phase, took)
	}
	_ = tw.Flush() // writing to a strings.Builder cannot fail

	out := "```\n" + sb.String() + "```"
	if hidden > 0 {
		out += fmt.Sprintf("\n_%d more nodes not shown_", hidden)
	}
	return out
}

func failureLines(nodes []rollout.Node) string {
	var lines []string
	failed := 0
	for i := range nodes {
		if nodes[i].Phase != patchv1alpha1.PatchJobPhaseFailed {
			continue
		}
		failed++
		if len(lines) >= maxFailureLines {
			continue
		}
		line := "*" + escapeMrkdwn.Replace(nodes[i].Name) + "*"
		if nodes[i].Message != "" {
			line += ": " + escapeMrkdwn.Replace(truncate(nodes[i].Message, maxFailureRunes))
		}
		lines = append(lines, line)
	}
	if failed > len(lines) {
		lines = append(lines, fmt.Sprintf("_and %d more failed nodes_", failed-len(lines)))
	}
	return strings.Join(lines, "\n")
}

// formatDuration shows d rounded to the second without trailing zero units, e.g. 1h12m or 45s.
func formatDuration(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

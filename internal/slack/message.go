package slack

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	slackgo "github.com/slack-go/slack"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/release"
)

const (
	// ApproveActionID and RejectActionID identify the buttons in interaction payloads.
	ApproveActionID = "approve_release"
	RejectActionID  = "reject_release"

	// A Block Kit section holds at most 3000 characters; the changelog is split over several.
	maxSectionRunes = 2900
	maxSections     = 4
)

var (
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdHeading = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*$`)
	mdBullet  = regexp.MustCompile(`(?m)^([ \t]*)[-*][ \t]+`)
	mdBold    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	// Bold module paths (the dependency list) would be auto-linked by Slack, taking the closing "*" along.
	mdBoldPath = regexp.MustCompile(`\*\*([^\s*]+/[^\s*]+)\*\*`)
	// Runs of padding spaces inside a line; leading indentation is kept.
	innerSpaces = regexp.MustCompile(`(\S)[ \t]{2,}`)
	// GitHub collapses these (the commit lists); they are shown as a title and grouped commits.
	htmlDetails = regexp.MustCompile(`(?s)<details>\s*<summary>(.*?)</summary>(.*?)</details>`)
	commitLine  = regexp.MustCompile(`^[ \t]*[*-][ \t]+([\w.-]+/[\w.-]+)@([0-9a-f]{7,40})[ \t]+(.+?)[ \t]*$`)
	commitType  = regexp.MustCompile(`^(\w+)(?:\(([^)]*)\))?!?:[ \t]+(.+)$`)
	htmlPara    = regexp.MustCompile(`</?p>`)
	blankLines  = regexp.MustCompile(`\n{3,}`)
	// "owner/repo@sha" as GitHub release notes write commits; not already part of a link.
	commitRef = regexp.MustCompile(`(^|[^\w|\[./-])([\w.-]+/[\w.-]+)@([0-9a-f]{7,40})\b`)
)

// Mrkdwn converts GitHub markdown to Slack mrkdwn.
func Mrkdwn(md string) string {
	s := strings.ReplaceAll(md, "\r\n", "\n")
	for _, title := range []string{"Contributors", "Images"} {
		s = dropSection(s, title)
	}
	s = htmlDetails.ReplaceAllStringFunc(s, func(block string) string {
		m := htmlDetails.FindStringSubmatch(block)
		return "\n**" + m[1] + "**\n" + groupCommits(m[2])
	})
	s = htmlPara.ReplaceAllString(s, "")
	s = escapeMrkdwn.Replace(s)
	s = mdLink.ReplaceAllString(s, "<$2|$1>")
	s = commitRef.ReplaceAllString(s, "$1<https://github.com/$2/commit/$3|$2@$3>")
	s = mdHeading.ReplaceAllString(s, "*$1*")
	s = mdBullet.ReplaceAllString(s, "$1• ")
	s = mdBoldPath.ReplaceAllString(s, "`$1`")
	s = mdBold.ReplaceAllString(s, "*$1*")
	s = innerSpaces.ReplaceAllString(s, "$1 ")
	s = blankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// groupCommits lists "owner/repo@sha message" lines by conventional-commit type, message first and
// the commit as a trailing link. Other lines are kept as they are.
func groupCommits(body string) string {
	titles := []string{"Features", "Fixes", "Other"}
	groups := map[string][]string{}
	var rest []string
	for _, line := range strings.Split(body, "\n") {
		m := commitLine.FindStringSubmatch(line)
		if m == nil {
			if line = htmlPara.ReplaceAllString(line, ""); strings.TrimSpace(line) != "" {
				rest = append(rest, line)
			}
			continue
		}
		group, msg := "Other", m[3]
		if t := commitType.FindStringSubmatch(msg); t != nil && (t[1] == "feat" || t[1] == "fix") {
			group, msg = map[string]string{"feat": "Features", "fix": "Fixes"}[t[1]], t[3]
			if t[2] != "" {
				msg = t[2] + ": " + msg
			}
		}
		groups[group] = append(groups[group], fmt.Sprintf("* %s [%s](https://github.com/%s/commit/%s)", msg, m[2], m[1], m[2]))
	}

	var out []string
	for _, title := range titles {
		if lines := groups[title]; len(lines) > 0 {
			out = append(out, fmt.Sprintf("**%s (%d)**", title, len(lines)), strings.Join(lines, "\n"), "")
		}
	}
	return strings.Join(append(out, rest...), "\n")
}

// changelogSections converts the release notes and splits them at line boundaries into Block Kit
// sized pieces. Notes beyond maxSections pieces are cut; the message links the full notes.
func changelogSections(md string) []string {
	var sections []string
	var cur []string
	size := 0
	for _, line := range strings.Split(Mrkdwn(md), "\n") {
		n := utf8.RuneCountInString(line) + 1
		if size+n > maxSectionRunes && len(cur) > 0 {
			sections = append(sections, strings.TrimSpace(strings.Join(cur, "\n")))
			cur, size = nil, 0
		}
		cur = append(cur, line)
		size += n
	}
	if len(cur) > 0 {
		sections = append(sections, strings.TrimSpace(strings.Join(cur, "\n")))
	}
	if len(sections) > maxSections {
		sections = append(sections[:maxSections-1:maxSections-1], strings.TrimRight(sections[maxSections-1], "\n")+"\n…")
	}
	return sections
}

// dropSection removes a markdown heading titled title and everything up to the next heading.
func dropSection(md, title string) string {
	var kept []string
	skipping := false
	for _, line := range strings.Split(md, "\n") {
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			skipping = strings.EqualFold(m[1], title)
		}
		if !skipping {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// buttonValue carries what the interaction handler needs to find the approval again.
func buttonValue(plan string, rec *patchv1alpha1.ApprovalRecord) string {
	return strings.Join([]string{plan, rec.ID, rec.Version}, "|")
}

// ParseButtonValue is the inverse of the value set on the buttons.
func ParseButtonValue(v string) (plan, id, version string, ok bool) {
	parts := strings.Split(v, "|")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

func mention(votes []patchv1alpha1.ApprovalVote) string {
	users := make([]string, len(votes))
	for i, v := range votes {
		users[i] = "<@" + v.UserID + ">"
	}
	return strings.Join(users, ", ")
}

func statusLine(rec *patchv1alpha1.ApprovalRecord) string {
	switch rec.State {
	case patchv1alpha1.ApprovalApproved:
		return fmt.Sprintf("*Release APPROVED:* %d/%d approvals received (%s).", len(rec.Approvals), rec.RequiredApprovals, mention(rec.Approvals))
	case patchv1alpha1.ApprovalRejected:
		return fmt.Sprintf("*Release REJECTED* by %s.", mention(rec.Rejections))
	case patchv1alpha1.ApprovalSuperseded:
		return "*Superseded* by a newer release."
	case patchv1alpha1.ApprovalExpired:
		return "*Expired:* no decision was made in time."
	case patchv1alpha1.ApprovalCancelled:
		return "*Cancelled:* the plan was cancelled."
	}
	line := fmt.Sprintf("*Approval status:* %d/%d", len(rec.Approvals), rec.RequiredApprovals)
	if len(rec.Approvals) > 0 {
		line += " (" + mention(rec.Approvals) + ")"
	}
	if rec.ExpiresAt != nil {
		line += fmt.Sprintf(", expires <!date^%d^{date_short_pretty} {time}|%s>", rec.ExpiresAt.Unix(), rec.ExpiresAt.UTC().Format("2006-01-02 15:04 MST"))
	}
	return line
}

const (
	maxPlanFields   = 7
	maxFieldRunes   = 200
	fieldLabelRunes = 60
)

var escapeMrkdwn = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// planFields returns the label and value of the plan's Slack field annotations, sorted by label.
// "cluster-name" becomes "Cluster name". Values are escaped so they cannot mention or link anything.
func planFields(plan *patchv1alpha1.PatchPlan) [][2]string {
	var fields [][2]string
	for key, value := range plan.Annotations {
		name, ok := strings.CutPrefix(key, patchv1alpha1.AnnotationSlackFieldPrefix)
		if !ok || name == "" || strings.TrimSpace(value) == "" {
			continue
		}
		label := strings.NewReplacer("-", " ", "_", " ").Replace(name)
		label = strings.ToUpper(label[:1]) + label[1:]
		fields = append(fields, [2]string{escapeMrkdwn.Replace(truncate(label, fieldLabelRunes)), escapeMrkdwn.Replace(truncate(strings.TrimSpace(value), maxFieldRunes))})
	}
	slices.SortFunc(fields, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
	return fields[:min(len(fields), maxPlanFields)]
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Blocks renders the approval message. rel may be nil, in which case the date, link and changelog
// are left out.
func Blocks(plan *patchv1alpha1.PatchPlan, rec *patchv1alpha1.ApprovalRecord, rel *release.Release) []slackgo.Block {
	text := func(s string) *slackgo.TextBlockObject {
		return slackgo.NewTextBlockObject(slackgo.MarkdownType, s, false, false)
	}

	lines := []string{"*Plan:* " + plan.Name}
	for _, f := range planFields(plan) {
		lines = append(lines, "*"+f[0]+":* "+f[1])
	}
	if rec.PreviousVersion != "" {
		lines = append(lines, "*Current version:* "+rec.PreviousVersion)
	}
	if rel != nil && !rel.PublishedAt.IsZero() {
		lines = append(lines, "*Released:* "+rel.PublishedAt.UTC().Format("2006-01-02"))
	}

	blocks := []slackgo.Block{
		slackgo.NewHeaderBlock(slackgo.NewTextBlockObject(slackgo.PlainTextType, "Talos "+rec.Version+" is available", false, false)),
		slackgo.NewSectionBlock(text(strings.Join(lines, "\n")), nil, nil),
	}
	if rel != nil {
		if sections := changelogSections(rel.Body); len(sections) > 0 {
			blocks = append(blocks, slackgo.NewDividerBlock())
			for _, section := range sections {
				blocks = append(blocks, slackgo.NewSectionBlock(text(section), nil, nil))
			}
		}
		if rel.URL != "" {
			blocks = append(blocks, slackgo.NewSectionBlock(text(fmt.Sprintf("<%s|View the full release notes on GitHub>", rel.URL)), nil, nil))
		}
	}
	blocks = append(blocks, slackgo.NewSectionBlock(text(statusLine(rec)), nil, nil))

	if rec.State == patchv1alpha1.ApprovalPending {
		value := buttonValue(plan.Name, rec)
		approve := slackgo.NewButtonBlockElement(ApproveActionID, value, slackgo.NewTextBlockObject(slackgo.PlainTextType, "Approve release", false, false))
		approve.Style = slackgo.StylePrimary
		reject := slackgo.NewButtonBlockElement(RejectActionID, value, slackgo.NewTextBlockObject(slackgo.PlainTextType, "Reject release", false, false))
		reject.Style = slackgo.StyleDanger
		blocks = append(blocks, slackgo.NewActionBlock("approval", approve, reject))
	}
	return blocks
}

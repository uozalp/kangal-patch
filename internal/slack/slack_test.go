package slack

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
)

var approvers = []string{"U1", "U2", "U3"}

func pending(required int32) *patchv1alpha1.ApprovalRecord {
	return &patchv1alpha1.ApprovalRecord{ID: "id1", Version: "v1.12.2", State: patchv1alpha1.ApprovalPending, RequiredApprovals: required}
}

func Test_Apply(t *testing.T) {
	now := time.Now()
	expired := metav1.NewTime(now.Add(-time.Minute))

	tests := []struct {
		name        string
		rec         func() *patchv1alpha1.ApprovalRecord
		user        string
		action      Action
		wantErr     error
		wantChanged bool
		wantState   patchv1alpha1.ApprovalState
	}{
		{name: "approver approves, threshold met", rec: func() *patchv1alpha1.ApprovalRecord { return pending(1) }, user: "U1", action: Approve, wantChanged: true, wantState: patchv1alpha1.ApprovalApproved},
		{name: "first of two approvals stays pending", rec: func() *patchv1alpha1.ApprovalRecord { return pending(2) }, user: "U1", action: Approve, wantChanged: true, wantState: patchv1alpha1.ApprovalPending},
		{name: "duplicate approval is a no-op", rec: func() *patchv1alpha1.ApprovalRecord {
			r := pending(2)
			r.Approvals = []patchv1alpha1.ApprovalVote{{UserID: "U1"}}
			return r
		}, user: "U1", action: Approve, wantState: patchv1alpha1.ApprovalPending},
		{name: "second approver completes", rec: func() *patchv1alpha1.ApprovalRecord {
			r := pending(2)
			r.Approvals = []patchv1alpha1.ApprovalVote{{UserID: "U1"}}
			return r
		}, user: "U2", action: Approve, wantChanged: true, wantState: patchv1alpha1.ApprovalApproved},
		{name: "unauthorized user approving", rec: func() *patchv1alpha1.ApprovalRecord { return pending(1) }, user: "U9", action: Approve, wantErr: ErrNotApprover, wantState: patchv1alpha1.ApprovalPending},
		{name: "unauthorized user rejecting", rec: func() *patchv1alpha1.ApprovalRecord { return pending(1) }, user: "U9", action: Reject, wantErr: ErrNotApprover, wantState: patchv1alpha1.ApprovalPending},
		{name: "one rejection is final", rec: func() *patchv1alpha1.ApprovalRecord { return pending(2) }, user: "U1", action: Reject, wantChanged: true, wantState: patchv1alpha1.ApprovalRejected},
		{name: "approve after reject", rec: func() *patchv1alpha1.ApprovalRecord {
			r := pending(1)
			r.State = patchv1alpha1.ApprovalRejected
			return r
		}, user: "U1", action: Approve, wantState: patchv1alpha1.ApprovalRejected},
		{name: "reject after approve", rec: func() *patchv1alpha1.ApprovalRecord {
			r := pending(1)
			r.State = patchv1alpha1.ApprovalApproved
			return r
		}, user: "U1", action: Reject, wantState: patchv1alpha1.ApprovalApproved},
		{name: "decision after expiry", rec: func() *patchv1alpha1.ApprovalRecord {
			r := pending(1)
			r.ExpiresAt = &expired
			return r
		}, user: "U1", action: Approve, wantState: patchv1alpha1.ApprovalPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := tt.rec()
			changed, err := Apply(rec, approvers, tt.user, tt.action, now)
			if err != tt.wantErr || changed != tt.wantChanged || rec.State != tt.wantState {
				t.Fatalf("changed=%t err=%v state=%s, want changed=%t err=%v state=%s", changed, err, rec.State, tt.wantChanged, tt.wantErr, tt.wantState)
			}
		})
	}
}

func Test_ArchiveAndExcluded(t *testing.T) {
	st := &patchv1alpha1.ApprovalStatus{}
	for i := range MaxHistory + 3 {
		rec := pending(1)
		rec.Version = "v1.0." + string(rune('a'+i))
		rec.State = patchv1alpha1.ApprovalRejected
		st.Current = rec
		Archive(st)
	}
	if st.Current != nil || len(st.History) != MaxHistory || st.History[0].Version != "v1.0.m" {
		t.Fatalf("history len=%d first=%s current=%v", len(st.History), st.History[0].Version, st.Current)
	}

	st.Current = pending(1)
	st.Current.State = patchv1alpha1.ApprovalExpired
	if ex := Excluded(st); !ex["v1.12.2"] || !ex["v1.0.m"] || len(ex) != MaxHistory+1 {
		t.Fatalf("excluded = %v", ex)
	}
}

func Test_Mrkdwn(t *testing.T) {
	got := Mrkdwn("## Notes\n\n* **Fix** a <thing> & [more](https://x.io/a)\n- second")
	want := "*Notes*\n\n• *Fix* a &lt;thing&gt; &amp; <https://x.io/a|more>\n• second"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	if got := Mrkdwn("## Notes\nkept\n## Contributors\n* Ann\n## Images\n```\nimg:1\n```\n## Changes\nalso kept"); got != "*Notes*\nkept\n*Changes*\nalso kept" {
		t.Fatalf("contributors or images not dropped: %q", got)
	}
}

func Test_MrkdwnCommitLists(t *testing.T) {
	got := Mrkdwn("## Changes\n<details><summary>4 commits</summary>\n<p>\n\n" +
		"* siderolabs/talos@c31b89093 feat: include applesmc module\n" +
		"* siderolabs/talos@b42de4573 fix(net): update GoBGP & friends\n" +
		"* siderolabs/talos@52417038c chore: rekres\n" +
		"* siderolabs/talos@ede45c42d release(v1.14.6): prepare release\n" +
		"</p>\n</details>")
	want := "*Changes*\n\n*4 commits*\n*Features (1)*\n• include applesmc module <https://github.com/siderolabs/talos/commit/c31b89093|c31b89093>\n\n" +
		"*Fixes (1)*\n• net: update GoBGP &amp; friends <https://github.com/siderolabs/talos/commit/b42de4573|b42de4573>\n\n" +
		"*Other (2)*\n• chore: rekres <https://github.com/siderolabs/talos/commit/52417038c|52417038c>\n" +
		"• release(v1.14.6): prepare release <https://github.com/siderolabs/talos/commit/ede45c42d|ede45c42d>"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func Test_MrkdwnDependencyList(t *testing.T) {
	got := Mrkdwn("* **github.com/gopacket/gopacket**  v1.7.1 -> v1.7.2")
	if want := "• `github.com/gopacket/gopacket` v1.7.1 -&gt; v1.7.2"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func Test_ChangelogSections(t *testing.T) {
	if got := changelogSections("short"); len(got) != 1 || got[0] != "short" {
		t.Fatalf("got %q", got)
	}

	sections := changelogSections(strings.Repeat("* siderolabs/talos@c31b89093 a fix\n", 1000))
	if len(sections) != maxSections || !strings.HasSuffix(sections[len(sections)-1], "\n…") {
		t.Fatalf("got %d sections, want %d with a cut-off marker", len(sections), maxSections)
	}
	for _, sec := range sections {
		if n := len([]rune(sec)); n > 3000 || strings.Count(sec, "<") != strings.Count(sec, ">") {
			t.Fatalf("section of %d runes or with a split link", n)
		}
	}
}

func Test_BlocksButtonsOnlyWhilePending(t *testing.T) {
	hasActions := func(rec *patchv1alpha1.ApprovalRecord) bool {
		for _, b := range Blocks(&patchv1alpha1.PatchPlan{}, rec, nil) {
			if b.BlockType() == slackgo.MBTAction {
				return true
			}
		}
		return false
	}
	rec := pending(2)
	if !hasActions(rec) {
		t.Fatal("pending message has no buttons")
	}
	rec.State = patchv1alpha1.ApprovalApproved
	if hasActions(rec) {
		t.Fatal("decided message still has buttons")
	}
}

func Test_PlanFields(t *testing.T) {
	plan := &patchv1alpha1.PatchPlan{ObjectMeta: metav1.ObjectMeta{Name: "p", Annotations: map[string]string{
		patchv1alpha1.AnnotationSlackFieldPrefix + "environment":  "Production",
		patchv1alpha1.AnnotationSlackFieldPrefix + "cluster-name": "talos <!channel> & co",
		patchv1alpha1.AnnotationSlackFieldPrefix + "empty":        " ",
		"unrelated": "ignored",
	}}}
	got := planFields(plan)
	want := [][2]string{{"Cluster name", "talos &lt;!channel&gt; &amp; co"}, {"Environment", "Production"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, want %v", got, want)
	}

	many := &patchv1alpha1.PatchPlan{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	for i := range 20 {
		many.Annotations[fmt.Sprintf("%sf%02d", patchv1alpha1.AnnotationSlackFieldPrefix, i)] = "v"
	}
	if n := len(planFields(many)); n != maxPlanFields {
		t.Fatalf("fields = %d, want %d", n, maxPlanFields)
	}
}

func Test_ConfigValidate(t *testing.T) {
	ok := Config{BotToken: "xoxb-a", AppToken: "xapp-a", ChannelID: "C1", Approvers: []string{"U1", "W2"}, Required: 2}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"missing token":      func(c *Config) { c.BotToken = "" },
		"bad approver":       func(c *Config) { c.Approvers = []string{"alice"} },
		"required too large": func(c *Config) { c.Required = 3 },
		"required zero":      func(c *Config) { c.Required = 0 },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if got := ParseApprovers(" U1,U2, ,U1"); len(got) != 2 {
		t.Fatalf("ParseApprovers = %v", got)
	}
}

func click(user, action, plan, id, version string) slackgo.InteractionCallback {
	cb := slackgo.InteractionCallback{Type: slackgo.InteractionTypeBlockActions}
	cb.User.ID = user
	cb.Container.ChannelID = "C1"
	cb.Container.MessageTs = "1.1"
	cb.ActionCallback.BlockActions = []*slackgo.BlockAction{{ActionID: action, Value: strings.Join([]string{plan, id, version}, "|")}}
	return cb
}

func Test_ListenerHandle(t *testing.T) {
	ctx := context.Background()
	rec := pending(2)
	rec.Slack = &patchv1alpha1.SlackMessageRef{ChannelID: "C1", MessageTS: "1.1"}
	plan := &patchv1alpha1.PatchPlan{
		ObjectMeta: metav1.ObjectMeta{Name: "auto"},
		Status:     patchv1alpha1.PatchPlanStatus{AutoUpdate: &patchv1alpha1.AutoUpdateStatus{Approval: &patchv1alpha1.ApprovalStatus{Current: rec}}},
	}
	scheme := runtime.NewScheme()
	if err := patchv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(plan).WithObjects(plan).Build()

	var staleUpdates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		staleUpdates.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	l := &Listener{
		Config: Config{Approvers: approvers},
		Slack:  &Client{api: slackgo.New("xoxb-test", slackgo.OptionAPIURL(srv.URL+"/"))},
		Reader: c,
		Writer: c,
	}
	state := func() *patchv1alpha1.ApprovalRecord {
		var got patchv1alpha1.PatchPlan
		if err := c.Get(ctx, client.ObjectKeyFromObject(plan), &got); err != nil {
			t.Fatal(err)
		}
		return got.Status.AutoUpdate.Approval.Current
	}

	if err := l.Handle(ctx, click("U9", ApproveActionID, "auto", "id1", "v1.12.2")); err != ErrNotApprover {
		t.Fatalf("unauthorized click: err=%v", err)
	}
	if err := l.Handle(ctx, click("U1", ApproveActionID, "auto", "other-id", "v1.12.2")); err == nil || staleUpdates.Load() != 1 {
		t.Fatalf("stale click: err=%v slack updates=%d, want the message retired", err, staleUpdates.Load())
	}
	if len(state().Approvals) != 0 {
		t.Fatal("stale click recorded a vote")
	}

	for _, user := range []string{"U1", "U1", "U2"} { // a duplicate delivery must not count twice
		if err := l.Handle(ctx, click(user, ApproveActionID, "auto", "id1", "v1.12.2")); err != nil {
			t.Fatal(err)
		}
	}
	if got := state(); got.State != patchv1alpha1.ApprovalApproved || len(got.Approvals) != 2 {
		t.Fatalf("state=%s approvals=%d", got.State, len(got.Approvals))
	}
}

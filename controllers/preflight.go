package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
	"github.com/uozalp/kangal-patch/internal/patchutil"
	"github.com/uozalp/kangal-patch/internal/registry"
	"github.com/uozalp/kangal-patch/internal/scheduling"
	"github.com/uozalp/kangal-patch/internal/supportmatrix"
	"github.com/uozalp/kangal-patch/internal/talos"
)

// Reasons set on the PreflightPassed condition.
const (
	reasonPreflightPassed         = "PreflightPassed"
	reasonNoNodesSelected         = "NoNodesSelected"
	reasonPlanConflict            = "PlanConflict"
	reasonKubernetesAPIUnhealthy  = "KubernetesAPIUnhealthy"
	reasonTalosCredentialsInvalid = "TalosCredentialsInvalid"
	reasonNodeUnreachable         = "NodeUnreachable"
	reasonInvalidTarget           = "InvalidTarget"
	reasonTalosImageUnavailable   = "TalosImageUnavailable"
	reasonUnsupportedVersionCombo = "UnsupportedVersionCombination"
	reasonControlPlaneOrder       = "ControlPlaneOrderRequired"
)

const (
	requeueWhenPreflightFailed    = time.Minute // re-run so fixing the cause (e.g. a Secret) self-heals
	preflightCallTimeout          = 15 * time.Second
	preflightImageCheckTimeout    = 2 * time.Minute // factory.talos.dev builds an image on its first request
	preflightNodeProbeConcurrency = 10
	preflightMaxNamesInMessages   = 20
)

type preflightFailure struct {
	reason  string
	message string
}

type preflightResult struct {
	failures []preflightFailure
	passed   []string
}

func (p *preflightResult) fail(reason, format string, args ...any) {
	p.failures = append(p.failures, preflightFailure{reason: reason, message: fmt.Sprintf(format, args...)})
}

func (p *preflightResult) pass(format string, args ...any) {
	p.passed = append(p.passed, fmt.Sprintf(format, args...))
}

// nodeProbe is what the Talos API and the Kubernetes Node reported for a single node.
type nodeProbe struct {
	node              string
	version           string
	kubernetesVersion string
	schematicID       string
}

// ensurePreflight runs the pre-flight checks once, before the first PatchJob of the plan is created,
// and reports whether scheduling may proceed. The result is recorded in the PreflightPassed condition;
// a failed run sets the plan to Failed and the caller retries until it passes.
func (r *PatchPlanReconciler) ensurePreflight(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, rollout *scheduling.Rollout, jobsByNode map[string]*patchv1alpha1.PatchJob) (bool, error) {
	// Once rollout has started nodes are expected to be cordoned/rebooting, so re-checking would be misleading.
	if len(jobsByNode) > 0 {
		return true, nil
	}

	cond := meta.FindStatusCondition(patchPlan.Status.Conditions, patchv1alpha1.ConditionPreflightPassed)
	if cond != nil && cond.Status == metav1.ConditionTrue && cond.ObservedGeneration == patchPlan.Generation {
		return true, nil
	}

	logger := log.FromContext(ctx)

	if patchPlan.Status.Phase == "" || patchPlan.Status.Phase == patchv1alpha1.PatchPhasePending {
		original := patchPlan.DeepCopy()
		patchPlan.Status.Phase = patchv1alpha1.PatchPhasePreflighting
		patchPlan.Status.Message = "Running preflight checks"
		if err := r.patchStatus(ctx, original, patchPlan); err != nil {
			return false, err
		}
	}

	res, err := r.runPreflight(ctx, patchPlan, rollout)
	if err != nil {
		return false, err
	}
	failures := res.failures

	original := patchPlan.DeepCopy()

	if len(failures) > 0 {
		messages := make([]string, len(failures))
		for i, f := range failures {
			messages[i] = f.message
		}
		summary := strings.Join(messages, "; ")

		meta.SetStatusCondition(&patchPlan.Status.Conditions, metav1.Condition{
			Type:               patchv1alpha1.ConditionPreflightPassed,
			Status:             metav1.ConditionFalse,
			Reason:             failures[0].reason,
			Message:            summary,
			ObservedGeneration: patchPlan.Generation,
		})
		patchPlan.Status.Phase = patchv1alpha1.PatchPhaseFailed
		patchPlan.Status.Message = "Preflight failed: " + summary

		logger.Info("preflight failed", "failures", messages, "passed", res.passed)
		return false, r.patchStatus(ctx, original, patchPlan)
	}

	summary := strings.Join(res.passed, "; ")

	meta.SetStatusCondition(&patchPlan.Status.Conditions, metav1.Condition{
		Type:               patchv1alpha1.ConditionPreflightPassed,
		Status:             metav1.ConditionTrue,
		Reason:             reasonPreflightPassed,
		Message:            summary,
		ObservedGeneration: patchPlan.Generation,
	})
	patchPlan.Status.Phase = patchv1alpha1.PatchPhasePending
	patchPlan.Status.Message = "Preflight passed: " + summary

	logger.Info("preflight passed", "checks", res.passed)
	return true, r.patchStatus(ctx, original, patchPlan)
}

// runPreflight executes every check and returns the passed and failed results. A non-nil error means
// a check couldn't be evaluated for a transient reason and should be retried rather than reported as a plan failure.
func (r *PatchPlanReconciler) runPreflight(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, rollout *scheduling.Rollout) (*preflightResult, error) {
	res := &preflightResult{}

	targetNodes := rollout.Nodes()
	if len(targetNodes) == 0 {
		if rollout.Selected == 0 {
			res.fail(reasonNoNodesSelected, "nodeSelector %s matched no nodes", metav1.FormatLabelSelector(patchPlan.Spec.NodeSelector))
		} else {
			res.fail(reasonNoNodesSelected, "none of the %d selected node(s) matches a group in strategy.order [%s]",
				rollout.Selected, strings.Join(scheduling.Order(patchPlan.Spec), ", "))
		}
		return res, nil
	}

	names := make([]string, len(targetNodes))
	for i := range targetNodes {
		names[i] = targetNodes[i].Name
	}
	res.pass("%d node(s) selected: %s", len(names), truncatedList(names, preflightMaxNamesInMessages))
	res.pass("rollout plan: %s", describeRollout(rollout))
	if len(rollout.Unassigned) > 0 {
		res.pass("%d selected node(s) match no group in strategy.order and are left untouched: %s",
			len(rollout.Unassigned), truncatedList(rollout.Unassigned, preflightMaxNamesInMessages))
	}

	checkUpgradeOrder(patchPlan.Spec.Target, rollout, res)

	if err := r.checkConflictingPlans(ctx, patchPlan, targetNodes, res); err != nil {
		return nil, err
	}

	r.checkKubernetesAPI(ctx, res)

	probes := r.checkTalosNodes(ctx, patchPlan, targetNodes, res)

	checkTalosImages(ctx, patchPlan.Spec.Target, probes, res)
	checkVersionCompatibility(ctx, patchPlan.Spec.Target, probes, res)

	return res, nil
}

// describeRollout summarizes the node count and concurrency of every group in schedule order.
func describeRollout(rollout *scheduling.Rollout) string {
	parts := make([]string, len(rollout.Groups))
	for i, g := range rollout.Groups {
		parts[i] = fmt.Sprintf("%s: %d node(s), concurrency %d", g.Name, len(g.Nodes), g.Concurrency)
	}
	return strings.Join(parts, "; ")
}

// checkUpgradeOrder rejects a strategy.order that can't carry the requested upgrade. A Kubernetes
// upgrade needs every control plane node upgraded before any worker; the configured order is never
// silently changed.
func checkUpgradeOrder(target patchv1alpha1.TargetSpec, rollout *scheduling.Rollout, res *preflightResult) {
	if target.KubernetesVersion == "" {
		return
	}
	if err := rollout.CheckControlPlaneFirst(); err != nil {
		res.fail(reasonControlPlaneOrder,
			"the requested Kubernetes upgrade requires control plane nodes to be upgraded before worker nodes, but %v; no PatchJobs have been created", err)
		return
	}
	res.pass("strategy.order upgrades control plane nodes before worker nodes")
}

// checkConflictingPlans fails if another active PatchPlan targets any of the same nodes.
func (r *PatchPlanReconciler) checkConflictingPlans(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, targetNodes []corev1.Node, res *preflightResult) error {
	var plans patchv1alpha1.PatchPlanList
	if err := r.List(ctx, &plans); err != nil {
		return fmt.Errorf("failed to list PatchPlans: %w", err)
	}

	mine := make(map[string]struct{}, len(targetNodes))
	for i := range targetNodes {
		mine[targetNodes[i].Name] = struct{}{}
	}

	conflicts := 0
	for i := range plans.Items {
		other := &plans.Items[i]
		if other.Name == patchPlan.Name || !other.DeletionTimestamp.IsZero() || other.IsAutoUpdateTemplate() {
			continue
		}
		if other.Status.Phase != patchv1alpha1.PatchPhaseInProgress && other.Status.Phase != patchv1alpha1.PatchPhasePaused {
			continue
		}

		nodes, err := nodeutil.ListMatchingNodes(ctx, r.Client, other.Spec.NodeSelector)
		if err != nil {
			return err
		}
		otherRollout, err := scheduling.Resolve(nodes, other.Spec)
		if err != nil {
			// An invalid plan fails its own preflight and schedules nothing
			continue
		}

		var overlap []string
		for _, n := range otherRollout.Nodes() {
			if _, ok := mine[n.Name]; ok {
				overlap = append(overlap, n.Name)
			}
		}
		if len(overlap) > 0 {
			res.fail(reasonPlanConflict, "PatchPlan %s is %s and targets the same node(s): %s",
				other.Name, other.Status.Phase, truncatedList(overlap, preflightMaxNamesInMessages))
			conflicts++
		}
	}

	if conflicts == 0 {
		res.pass("no other active PatchPlan targets the same nodes")
	}
	return nil
}

// checkKubernetesAPI verifies the API server reports ready.
func (r *PatchPlanReconciler) checkKubernetesAPI(ctx context.Context, res *preflightResult) {
	if r.Discovery == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, preflightCallTimeout)
	defer cancel()

	if _, err := r.Discovery.RESTClient().Get().AbsPath("/readyz").DoRaw(ctx); err != nil {
		res.fail(reasonKubernetesAPIUnhealthy, "Kubernetes API /readyz check failed: %v", err)
		return
	}
	res.pass("Kubernetes API /readyz is healthy")
}

// checkTalosNodes verifies the Talos credentials work and every selected node answers on the Talos
// API. It returns what each reachable node reported, or nil if the Talos API can't be used at all.
func (r *PatchPlanReconciler) checkTalosNodes(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, targetNodes []corev1.Node, res *preflightResult) []nodeProbe {
	logger := log.FromContext(ctx)

	talosConfig, err := resolveTalosConfig(ctx, r.Client, patchPlan)
	if err != nil {
		res.fail(reasonTalosCredentialsInvalid, "%v", err)
		return nil
	}

	talosClient, err := talos.NewClient(talosConfig)
	if err != nil {
		res.fail(reasonTalosCredentialsInvalid, "failed to create Talos client: %v", err)
		return nil
	}
	defer func() {
		if cerr := talosClient.Close(); cerr != nil {
			logger.Error(cerr, "failed to close Talos client")
		}
	}()

	connCtx, cancel := context.WithTimeout(ctx, preflightCallTimeout)
	defer cancel()
	if err := talosClient.CheckConnection(connCtx); err != nil {
		res.fail(reasonTalosCredentialsInvalid, "%v", err)
		return nil
	}
	res.pass("Talos credentials accepted by endpoints %s", strings.Join(talosConfig.Endpoints, ", "))

	target := patchPlan.Spec.Target
	needSchematic := target.TalosVersion != "" && target.Source == "factory" && target.SchematicID == ""

	type outcome struct {
		probe nodeProbe
		err   error
	}
	outcomes := make([]outcome, len(targetNodes))
	sem := make(chan struct{}, preflightNodeProbeConcurrency)
	var wg sync.WaitGroup
	for i := range targetNodes {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			probe, err := probeNode(ctx, talosClient, &targetNodes[i], needSchematic)
			outcomes[i] = outcome{probe: probe, err: err}
		})
	}
	wg.Wait()

	var probes []nodeProbe
	unreachable := 0
	for i, o := range outcomes {
		if o.err != nil {
			res.fail(reasonNodeUnreachable, "node %s is not reachable via the Talos API: %v", targetNodes[i].Name, o.err)
			unreachable++
			continue
		}
		probes = append(probes, o.probe)
	}
	if unreachable == 0 {
		res.pass("all %d node(s) reachable via the Talos API", len(probes))
	}
	return probes
}

func probeNode(ctx context.Context, talosClient *talos.Client, node *corev1.Node, needSchematic bool) (nodeProbe, error) {
	addr, err := nodeutil.GetNodeInternalIP(node)
	if err != nil {
		return nodeProbe{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, preflightCallTimeout)
	defer cancel()

	probe := nodeProbe{node: node.Name, kubernetesVersion: node.Status.NodeInfo.KubeletVersion}
	if probe.version, err = talosClient.GetVersion(ctx, addr); err != nil {
		return nodeProbe{}, err
	}
	if needSchematic {
		if probe.schematicID, err = talosClient.GetSchematicID(ctx, addr); err != nil {
			return nodeProbe{}, err
		}
	}
	return probe, nil
}

// checkTalosImages verifies the installer image exists for every node that still needs the Talos upgrade.
func checkTalosImages(ctx context.Context, target patchv1alpha1.TargetSpec, probes []nodeProbe, res *preflightResult) {
	if target.TalosVersion == "" {
		return
	}

	images := map[string][]string{}
	invalid := false
	for _, p := range probes {
		if p.version == target.TalosVersion {
			continue
		}
		image, err := patchutil.BuildInstallerImage(target, p.schematicID)
		if err != nil {
			res.fail(reasonInvalidTarget, "node %s: %v", p.node, err)
			invalid = true
			continue
		}
		images[image] = append(images[image], p.node)
	}

	imageNames := make([]string, 0, len(images))
	for image := range images {
		imageNames = append(imageNames, image)
	}
	slices.Sort(imageNames)

	if len(imageNames) == 0 {
		if !invalid {
			res.pass("no node needs the Talos upgrade to %s", target.TalosVersion)
		}
		return
	}

	checker := registry.Checker{HTTPClient: &http.Client{Timeout: preflightImageCheckTimeout}}
	for _, image := range imageNames {
		err := checker.Exists(ctx, image)
		switch {
		case err == nil:
			res.pass("installer image %s exists", image)
		case errors.Is(err, registry.ErrNotFound):
			res.fail(reasonTalosImageUnavailable, "installer image %s does not exist (needed by %s)",
				image, truncatedList(images[image], preflightMaxNamesInMessages))
		default:
			res.fail(reasonTalosImageUnavailable, "unable to verify installer image %s: %v", image, err)
		}
	}
}

// versionPair is a Talos and Kubernetes version running together on a node.
type versionPair struct{ talos, kubernetes string }

func (v versionPair) String() string {
	return fmt.Sprintf("Talos %s + Kubernetes %s", v.talos, v.kubernetes)
}

// checkVersionCompatibility treats the support matrix as a matrix, per distinct combination of
// versions found on the nodes: the current combination is reported, and the combination each node
// ends up with once the plan is done must be supported. A Talos-only upgrade is therefore also
// checked against the kubelet version the node keeps running.
func checkVersionCompatibility(ctx context.Context, target patchv1alpha1.TargetSpec, probes []nodeProbe, res *preflightResult) {
	type transition struct{ current, desired versionPair }

	seen := map[transition]struct{}{}
	var transitions []transition
	for _, p := range probes {
		current := versionPair{talos: p.version, kubernetes: p.kubernetesVersion}
		desired := versionPair{
			talos:      firstNonEmpty(target.TalosVersion, p.version),
			kubernetes: firstNonEmpty(target.KubernetesVersion, p.kubernetesVersion),
		}
		t := transition{current, desired}
		if _, ok := seen[t]; ok || desired.kubernetes == "" || desired.talos == "" {
			continue
		}
		seen[t] = struct{}{}
		transitions = append(transitions, t)
	}
	slices.SortFunc(transitions, func(a, b transition) int {
		return strings.Compare(a.current.String()+a.desired.String(), b.current.String()+b.desired.String())
	})

	logger := log.FromContext(ctx)
	for _, t := range transitions {
		if t.current.kubernetes != "" {
			if err := supportmatrix.ValidateKubernetesVersion(t.current.talos, t.current.kubernetes); err != nil && !errors.Is(err, supportmatrix.ErrUnknownTalosVersion) {
				res.pass("current combination %s is outside the support matrix (%v)", t.current, err)
			}
		}
		if t.current == t.desired {
			continue
		}

		err := supportmatrix.ValidateKubernetesVersion(t.desired.talos, t.desired.kubernetes)
		switch {
		case err == nil:
			res.pass("target combination %s is supported (from %s)", t.desired, t.current)
		case errors.Is(err, supportmatrix.ErrUnknownTalosVersion):
			res.pass("target combination %s not verified (Talos version missing from the matrix)", t.desired)
			logger.Info("skipping version compatibility check, Talos version missing from the matrix", "talosVersion", t.desired.talos)
		default:
			res.fail(reasonUnsupportedVersionCombo, "target combination %s is invalid (from %s): %v", t.desired, t.current, err)
		}
	}
}

// truncatedList joins up to limit items, noting how many were omitted.
func truncatedList(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:limit], ", "), len(items)-limit)
}

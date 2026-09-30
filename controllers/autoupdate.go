package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	"github.com/uozalp/kangal-patch/internal/nodeutil"
	"github.com/uozalp/kangal-patch/internal/release"
	"github.com/uozalp/kangal-patch/internal/talos"
)

const (
	minAutoUpdateCheckInterval = time.Minute
	requeueAutoUpdateRetry     = 5 * time.Minute // retry sooner than checkInterval when a check fails
	autoUpdateCheckTimeout     = 2 * time.Minute

	waitingMessagePrefix = "Waiting for PatchPlan"
)

// releaseLister provides the stable Talos releases an auto-update chooses from.
type releaseLister interface {
	List(ctx context.Context) ([]release.Release, error)
}

// reconcileAutoUpdate handles an auto-update template. It never patches nodes: when a newer release
// qualifies and no earlier child plan is still unfinished, it creates a child PatchPlan with
// target.talosVersion set, which then runs through the normal flow.
func (r *PatchPlanReconciler) reconcileAutoUpdate(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	spec := patchPlan.Spec.Target.AutoUpdate

	interval := max(spec.CheckInterval.Duration, minAutoUpdateCheckInterval)

	if patchPlan.Spec.Cancelled {
		return ctrl.Result{}, r.updateTemplateStatus(ctx, patchPlan, patchv1alpha1.PatchPhaseCancelled, "Auto-update cancelled by user", nil)
	}
	if patchPlan.Spec.Paused {
		err := r.updateTemplateStatus(ctx, patchPlan, patchv1alpha1.PatchPhasePaused, "Auto-update paused by user", nil)
		return ctrl.Result{RequeueAfter: requeueWhenPaused}, err
	}

	if patchPlan.Status.Phase != patchv1alpha1.PatchPhaseWatching {
		if err := r.updateTemplateStatus(ctx, patchPlan, patchv1alpha1.PatchPhaseWatching, "Watching for new Talos releases", nil); err != nil {
			return ctrl.Result{}, err
		}
	}

	var children patchv1alpha1.PatchPlanList
	if err := r.List(ctx, &children, client.MatchingLabels{patchv1alpha1.LabelParentPlan: patchPlan.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list child PatchPlans: %w", err)
	}
	for i := range children.Items {
		child := &children.Items[i]
		if child.Status.Phase == patchv1alpha1.PatchPhaseCompleted {
			continue
		}
		message := fmt.Sprintf("%s %s (%s) to complete before creating another", waitingMessagePrefix, child.Name, phaseOrPending(child.Status.Phase))
		err := r.updateTemplateStatus(ctx, patchPlan, patchv1alpha1.PatchPhaseWatching, message, nil)
		return ctrl.Result{RequeueAfter: interval}, err
	}

	if last := patchPlan.Status.AutoUpdate; last != nil && last.LastCheckTime != nil {
		if wait := interval - time.Since(last.LastCheckTime.Time); wait > 0 {
			var err error
			if strings.HasPrefix(patchPlan.Status.Message, waitingMessagePrefix) {
				next := last.LastCheckTime.Add(interval).UTC().Format(time.RFC3339)
				err = r.updateTemplateStatus(ctx, patchPlan, patchv1alpha1.PatchPhaseWatching, "Next release check at "+next, nil)
			}
			return ctrl.Result{RequeueAfter: wait}, err
		}
	}

	checkCtx, cancel := context.WithTimeout(ctx, autoUpdateCheckTimeout)
	defer cancel()
	current, result, err := r.checkForRelease(checkCtx, patchPlan, spec)
	if err != nil {
		logger.Info("auto-update release check failed", "error", err.Error())
		statusErr := r.updateTemplateStatus(ctx, patchPlan, patchv1alpha1.PatchPhaseWatching, "Release check failed: "+err.Error(), nil)
		return ctrl.Result{RequeueAfter: min(interval, requeueAutoUpdateRetry)}, statusErr
	}

	message := "Up to date"
	var createdPlan string
	switch {
	case result.Target != nil:
		version := result.Target.Version.String()
		name, created, err := r.createChildPlan(ctx, patchPlan, version)
		if err != nil {
			return ctrl.Result{}, err
		}
		if created {
			createdPlan = name
			message = fmt.Sprintf("Created PatchPlan %s for Talos %s", name, version)
			logger.Info("created child PatchPlan for new Talos release", "plan", name, "from", current, "to", version)
		} else {
			message = fmt.Sprintf("PatchPlan %s already exists but nodes still run Talos %s", name, current)
		}
	case result.Pending != nil:
		eligibleAt := result.Pending.PublishedAt.Add(spec.MinReleaseAge.Duration)
		message = fmt.Sprintf("Talos %s is newer than %s but not eligible until %s (minReleaseAge %s)",
			result.Pending.Version, current, eligibleAt.UTC().Format(time.RFC3339), spec.MinReleaseAge.Duration)
	}

	err = r.updateTemplateStatus(ctx, patchPlan, patchv1alpha1.PatchPhaseWatching, message, func(s *patchv1alpha1.AutoUpdateStatus) {
		s.LastCheckTime = &metav1.Time{Time: time.Now()}
		s.CurrentVersion = current.String()
		s.LatestVersion = versionString(result.Latest)
		s.PendingVersion = versionString(result.Pending)
		if createdPlan != "" {
			s.LastCreatedPlan = createdPlan
		}
	})
	return ctrl.Result{RequeueAfter: interval}, err
}

// checkForRelease determines the lowest Talos version on the selected nodes and which release, if
// any, should replace it.
func (r *PatchPlanReconciler) checkForRelease(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, spec *patchv1alpha1.AutoUpdateSpec) (release.Version, release.Result, error) {
	nodes, err := nodeutil.ListMatchingNodes(ctx, r.Client, patchPlan.Spec.NodeSelector)
	if err != nil {
		return release.Version{}, release.Result{}, err
	}
	controlPlane, workers := nodeutil.SplitByRole(nodes)
	targetNodes := nodeutil.OrderTargetNodes(controlPlane, workers, patchPlan.Spec)
	if len(targetNodes) == 0 {
		return release.Version{}, release.Result{}, fmt.Errorf("nodeSelector matched no nodes")
	}

	current, err := r.lowestTalosVersion(ctx, patchPlan, targetNodes)
	if err != nil {
		return release.Version{}, release.Result{}, err
	}

	releases, err := r.Releases.List(ctx)
	if err != nil {
		return release.Version{}, release.Result{}, err
	}

	return current, release.Select(releases, current, spec.Allow, spec.MinReleaseAge.Duration, time.Now()), nil
}

// lowestTalosVersion returns the lowest version reported by the Talos API across nodes.
func (r *PatchPlanReconciler) lowestTalosVersion(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, nodes []corev1.Node) (release.Version, error) {
	talosConfig, err := resolveTalosConfig(ctx, r.Client, patchPlan)
	if err != nil {
		return release.Version{}, err
	}

	talosClient, err := talos.NewClient(talosConfig)
	if err != nil {
		return release.Version{}, fmt.Errorf("failed to create Talos client: %w", err)
	}
	defer func() {
		if cerr := talosClient.Close(); cerr != nil {
			log.FromContext(ctx).Error(cerr, "failed to close Talos client")
		}
	}()

	var lowest release.Version
	for i := range nodes {
		probe, err := probeNode(ctx, talosClient, &nodes[i], false)
		if err != nil {
			return release.Version{}, fmt.Errorf("node %s is not reachable via the Talos API: %w", nodes[i].Name, err)
		}
		v, err := release.ParseVersion(probe.version)
		if err != nil {
			return release.Version{}, fmt.Errorf("node %s: %w", nodes[i].Name, err)
		}
		if i == 0 || v.Compare(lowest) < 0 {
			lowest = v
		}
	}
	return lowest, nil
}

// createChildPlan creates the PatchPlan that rolls out version. It reports false if the plan
// already exists.
func (r *PatchPlanReconciler) createChildPlan(ctx context.Context, parent *patchv1alpha1.PatchPlan, version string) (string, bool, error) {
	child := &patchv1alpha1.PatchPlan{
		ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("%s-%s", parent.Name, version),
			Labels: map[string]string{patchv1alpha1.LabelParentPlan: parent.Name},
		},
		Spec: *parent.Spec.DeepCopy(),
	}
	child.Spec.Target.AutoUpdate = nil
	child.Spec.Target.TalosVersion = version
	child.Spec.Paused = false
	child.Spec.Cancelled = false

	if err := ctrl.SetControllerReference(parent, child, r.Scheme); err != nil {
		return "", false, err
	}

	if err := r.Create(ctx, child); err != nil {
		if errors.IsAlreadyExists(err) {
			return child.Name, false, nil
		}
		return "", false, fmt.Errorf("failed to create PatchPlan %s: %w", child.Name, err)
	}
	return child.Name, true, nil
}

// updateTemplateStatus sets the phase and message of an auto-update template, applying mutate to
// its autoUpdate status first, and patches only if something changed.
func (r *PatchPlanReconciler) updateTemplateStatus(ctx context.Context, patchPlan *patchv1alpha1.PatchPlan, phase patchv1alpha1.PatchPhase, message string, mutate func(*patchv1alpha1.AutoUpdateStatus)) error {
	original := patchPlan.DeepCopy()

	patchPlan.Status.Phase = phase
	patchPlan.Status.Message = message
	if patchPlan.Status.AutoUpdate == nil {
		patchPlan.Status.AutoUpdate = &patchv1alpha1.AutoUpdateStatus{}
	}
	if mutate != nil {
		mutate(patchPlan.Status.AutoUpdate)
	}

	if equality.Semantic.DeepEqual(original.Status, patchPlan.Status) {
		return nil
	}
	return r.patchStatus(ctx, original, patchPlan)
}

func phaseOrPending(phase patchv1alpha1.PatchPhase) patchv1alpha1.PatchPhase {
	if phase == "" {
		return patchv1alpha1.PatchPhasePending
	}
	return phase
}

func versionString(r *release.Release) string {
	if r == nil {
		return ""
	}
	return r.Version.String()
}

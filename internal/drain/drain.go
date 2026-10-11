package drain

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CordonedByAnnotation marks a node this operator cordoned; only such nodes get uncordoned.
const CordonedByAnnotation = "kangalpatch.ozalp.dk/cordoned-by"

// Drainer handles node drain operations
type Drainer struct {
	client client.Client
}

// NewDrainer creates a new Drainer
func NewDrainer(c client.Client) *Drainer {
	return &Drainer{client: c}
}

// CordonNode marks a node as unschedulable
func (d *Drainer) CordonNode(ctx context.Context, nodeName string) error {
	var node corev1.Node
	if err := d.client.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if node.Spec.Unschedulable {
		return nil // cordoned by an admin or an earlier run; ownership is left unchanged
	}

	base := node.DeepCopy()
	node.Spec.Unschedulable = true
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[CordonedByAnnotation] = "true"
	if err := d.client.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("failed to cordon node: %w", err)
	}

	return nil
}

// UncordonNode marks a node as schedulable, but only if CordonNode cordoned it.
func (d *Drainer) UncordonNode(ctx context.Context, nodeName string) error {
	var node corev1.Node
	if err := d.client.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("failed to get node: %w", err)
	}

	if _, ours := node.Annotations[CordonedByAnnotation]; !ours {
		return nil
	}

	base := node.DeepCopy()
	node.Spec.Unschedulable = false
	delete(node.Annotations, CordonedByAnnotation)
	if err := d.client.Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("failed to uncordon node: %w", err)
	}

	return nil
}

// DefaultTimeout is used when DrainOptions.Timeout is not positive
const DefaultTimeout = 5 * time.Minute

// DrainOptions configures drain behavior
type DrainOptions struct {
	// RespectPDBs indicates whether to respect PodDisruptionBudgets
	RespectPDBs bool
	// Timeout is the maximum time to wait for drain
	Timeout time.Duration
}

// DrainNode evicts all pods from a node. It returns blocked=true when a PDB refused at least one
// eviction (RespectPDBs); the caller should retry later.
func (d *Drainer) DrainNode(ctx context.Context, nodeName string, opts DrainOptions) (blocked bool, err error) {
	pods, err := d.listPods(ctx, nodeName, shouldEvictPod)
	if err != nil {
		return false, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods) == 0 {
		return false, nil
	}

	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	for _, pod := range pods {
		podBlocked, err := d.evictPod(ctx, &pod, opts)
		if err != nil {
			return false, fmt.Errorf("failed to evict pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		blocked = blocked || podBlocked
	}

	return blocked, nil
}

// listPods returns the node's pods that match keep
func (d *Drainer) listPods(ctx context.Context, nodeName string, keep func(*corev1.Pod) bool) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := d.client.List(ctx, &podList, &client.ListOptions{
		FieldSelector: fields.SelectorFromSet(fields.Set{"spec.nodeName": nodeName}),
	}); err != nil {
		return nil, err
	}

	var pods []corev1.Pod
	for _, pod := range podList.Items {
		if keep(&pod) {
			pods = append(pods, pod)
		}
	}

	return pods, nil
}

// shouldEvictPod determines if a pod should be evicted; terminating pods were already evicted
func shouldEvictPod(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp == nil && blocksDrain(pod)
}

// blocksDrain reports whether the pod must be gone from the node before it counts as drained
func blocksDrain(pod *corev1.Pod) bool {
	// Skip completed/failed pods
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return false
	}

	// Skip DaemonSet pods
	if isDaemonSetPod(pod) {
		return false
	}

	// Skip mirror pods (static pods)
	if isMirrorPod(pod) {
		return false
	}

	return true
}

// isDaemonSetPod checks if a pod is managed by a DaemonSet
func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// isMirrorPod checks if a pod is a mirror pod (static pod)
func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return ok
}

// evictPod evicts a single pod; blocked=true means a PDB refused it and it should be retried
func (d *Drainer) evictPod(ctx context.Context, pod *corev1.Pod, opts DrainOptions) (blocked bool, err error) {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
	}

	err = d.client.SubResource("eviction").Create(ctx, pod, eviction)
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return false, nil
	case apierrors.IsTooManyRequests(err):
		if opts.RespectPDBs {
			return true, nil
		}
		return false, d.deletePod(ctx, pod)
	}
	return false, err
}

// deletePod forcefully deletes a pod when eviction is blocked by PDB
func (d *Drainer) deletePod(ctx context.Context, pod *corev1.Pod) error {
	if err := d.client.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to delete pod: %w", err)
	}

	return nil
}

// IsDrained checks if all evictable pods have been removed from the node
func (d *Drainer) IsDrained(ctx context.Context, nodeName string) (bool, error) {
	// Terminating pods still count: they may be mid-shutdown or stuck on a finalizer.
	pods, err := d.listPods(ctx, nodeName, blocksDrain)
	if err != nil {
		return false, err
	}
	return len(pods) == 0, nil
}

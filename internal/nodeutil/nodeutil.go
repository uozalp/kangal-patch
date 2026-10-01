package nodeutil

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// GetNodeInternalIP returns the node's InternalIP address. The Talos API is reached by
// address, not by the Kubernetes node name, which isn't guaranteed to be DNS-resolvable.
func GetNodeInternalIP(node *corev1.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("no InternalIP address found for node %s", node.Name)
}

// IsControlPlane checks if a node is a control plane node
func IsControlPlane(node *corev1.Node) bool {
	labels := node.GetLabels()
	if labels == nil {
		return false
	}

	if _, ok := labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}

	return false
}

// ListMatchingNodes retrieves all nodes matching the specified label selector.
// If nodeSelector is nil or empty, all nodes are returned.
func ListMatchingNodes(ctx context.Context, c client.Client, nodeSelector *metav1.LabelSelector) ([]corev1.Node, error) {
	logger := log.FromContext(ctx)

	if nodeSelector == nil {
		nodeSelector = &metav1.LabelSelector{} // a nil selector would match nothing
	}
	selector, err := metav1.LabelSelectorAsSelector(nodeSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid nodeSelector: %w", err)
	}

	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		logger.Error(err, "unable to list nodes")
		return nil, err
	}

	return nodeList.Items, nil
}

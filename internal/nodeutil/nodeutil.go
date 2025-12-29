/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodeutil

import (
	"context"

	patchv1alpha1 "github.com/uozalp/kangal-patch/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SplitByRole splits nodes into control plane and worker nodes
func SplitByRole(nodes []corev1.Node) (controlPlane, workers []corev1.Node) {
	for i := range nodes {
		node := &nodes[i]
		if IsControlPlane(node) {
			controlPlane = append(controlPlane, *node)
		} else {
			workers = append(workers, *node)
		}
	}
	return controlPlane, workers
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

// listMatchingNodes retrieves all nodes matching the specified label selector.
// If nodeSelector is empty, all nodes are returned.
func ListMatchingNodes(ctx context.Context, c client.Client, nodeSelector map[string]string) ([]corev1.Node, error) {
	logger := log.FromContext(ctx)

	nodeList := &corev1.NodeList{}
	listOpts := []client.ListOption{}
	if len(nodeSelector) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(nodeSelector))
	}

	if err := c.List(ctx, nodeList, listOpts...); err != nil {
		logger.Error(err, "unable to list nodes")
		return nil, err
	}

	return nodeList.Items, nil
}

// FindNextUnpatchedNode returns the first node from targetNodes that doesn't have a PatchJob yet.
// Returns nil if all nodes already have jobs.
func FindNextUnpatchedNode(targetNodes []corev1.Node, jobsByNode map[string]*patchv1alpha1.PatchJob) *corev1.Node {
	for i := range targetNodes {
		node := &targetNodes[i]
		if _, exists := jobsByNode[node.Name]; !exists {
			return node
		}
	}
	return nil
}

// OrderTargetNodes returns nodes in the order they should be patched based on the PatchPlan spec.
// It respects the ControlPlaneFirst flag and the PatchControlPlane/PatchWorkers settings.
func OrderTargetNodes(controlPlane, workers []corev1.Node, spec patchv1alpha1.PatchPlanSpec) []corev1.Node {
	var targetNodes []corev1.Node

	if spec.ControlPlaneFirst {
		if spec.PatchControlPlane {
			targetNodes = append(targetNodes, controlPlane...)
		}
		if spec.PatchWorkers {
			targetNodes = append(targetNodes, workers...)
		}
		return targetNodes
	}

	// Workers first
	if spec.PatchWorkers {
		targetNodes = append(targetNodes, workers...)
	}
	if spec.PatchControlPlane {
		targetNodes = append(targetNodes, controlPlane...)
	}
	return targetNodes
}

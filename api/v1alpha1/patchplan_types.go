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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PatchPlanSpec defines the desired state of PatchPlan
type PatchPlanSpec struct {
	// TargetVersion is the Talos version to upgrade to
	// +kubebuilder:validation:Required
	TargetVersion string `json:"targetVersion"`

	// NodeSelector selects which nodes to patch (label selector)
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// MaxConcurrency is the maximum number of nodes to patch concurrently
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MaxConcurrency int `json:"maxConcurrency,omitempty"`

	// MaxFailures is the maximum number of failed nodes before the plan stops.
	// Default is 0 (fail on first failure).
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	MaxFailures int `json:"maxFailures,omitempty"`

	// DelayBetweenNodes is the delay between patching individual nodes (e.g., "30s", "2m")
	// +kubebuilder:default="5m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(s|m|h))+$"
	DelayBetweenNodes metav1.Duration `json:"delayBetweenNodes,omitempty"`

	// RespectPDBs indicates whether to respect PodDisruptionBudgets during node drain
	// +kubebuilder:default=true
	RespectPDBs bool `json:"respectPDBs,omitempty"`

	// DrainTimeout is the maximum time to wait for node drain
	// +kubebuilder:default="5m"
	DrainTimeout metav1.Duration `json:"drainTimeout,omitempty"`

	// RebootTimeout is the maximum time to wait for node to reboot and become ready
	// +kubebuilder:default="10m"
	RebootTimeout metav1.Duration `json:"rebootTimeout,omitempty"`

	// PatchControlPlane indicates whether to patch control plane nodes
	// +kubebuilder:default=true
	PatchControlPlane bool `json:"patchControlPlane,omitempty"`

	// PatchWorkers indicates whether to patch worker nodes
	// +kubebuilder:default=true
	PatchWorkers bool `json:"patchWorkers,omitempty"`

	// ControlPlaneFirst indicates whether to patch control plane before workers
	// +kubebuilder:default=false
	ControlPlaneFirst bool `json:"controlPlaneFirst,omitempty"`

	// Paused pauses the patching operation
	// +kubebuilder:default=false
	Paused bool `json:"paused,omitempty"`

	// TalosConfig contains Talos API connection information
	// +optional
	TalosConfig TalosConfig `json:"talosConfig,omitempty"`
}

// TalosConfig contains configuration for connecting to Talos API
type TalosConfig struct {
	// Endpoints is a list of Talos API endpoints
	// +optional
	Endpoints []string `json:"endpoints,omitempty"`

	// CACert is the CA certificate for Talos API (base64 encoded)
	// +optional
	CACert string `json:"caCert,omitempty"`

	// ClientCert is the client certificate for Talos API (base64 encoded)
	// +optional
	ClientCert string `json:"clientCert,omitempty"`

	// ClientKey is the client key for Talos API (base64 encoded)
	// +optional
	ClientKey string `json:"clientKey,omitempty"`

	// SecretRef references a Secret containing Talos credentials
	// +optional
	SecretRef *SecretReference `json:"secretRef,omitempty"`
}

// SecretReference contains a reference to a Secret
type SecretReference struct {
	// Name is the name of the secret
	Name string `json:"name"`

	// Namespace is the namespace of the secret
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// PatchPlanStatus defines the observed state of PatchPlan
type PatchPlanStatus struct {
	// Phase represents the current phase of the patching operation
	// +kubebuilder:validation:Enum=Pending;InProgress;Paused;Completed;Failed
	Phase PatchPhase `json:"phase,omitempty"`

	// TargetVersion is the display version extracted from spec.targetVersion
	// +optional
	TargetVersion string `json:"targetVersion,omitempty"`

	// TotalNodes is the total number of nodes selected for patching
	TotalNodes int `json:"totalNodes,omitempty"`

	// CompletedNodes is the number of nodes successfully patched (derived from PatchJobs)
	// +kubebuilder:default=0
	CompletedNodes int `json:"completedNodes"`

	// FailedNodes is the number of nodes that failed to patch (derived from PatchJobs)
	// +kubebuilder:default=0
	FailedNodes int `json:"failedNodes"`

	// LastNodeScheduledAt is when the last PatchJob was created
	// Used to enforce DelayBetweenNodes
	// +optional
	LastNodeScheduledAt *metav1.Time `json:"lastNodeScheduledAt,omitempty"`

	// Message contains human-readable message about current state
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when the patching operation started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the patching operation completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Conditions represent the latest available observations of the PatchPlan's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PatchPhase represents the phase of patching operation
type PatchPhase string

const (
	PatchPhasePending    PatchPhase = "Pending"
	PatchPhaseInProgress PatchPhase = "InProgress"
	PatchPhasePaused     PatchPhase = "Paused"
	PatchPhaseCompleted  PatchPhase = "Completed"
	PatchPhaseFailed     PatchPhase = "Failed"
)

// +kubebuilder:object:root=true

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.targetVersion`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalNodes`
// +kubebuilder:printcolumn:name="Completed",type=integer,JSONPath=`.status.completedNodes`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failedNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PatchPlan is the Schema for the patchplans API
type PatchPlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PatchPlanSpec   `json:"spec,omitempty"`
	Status PatchPlanStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PatchPlanList contains a list of PatchPlan
type PatchPlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PatchPlan `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PatchPlan{}, &PatchPlanList{})
}

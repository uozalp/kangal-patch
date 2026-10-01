package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PatchJobSpec defines the desired state of PatchJob
// +kubebuilder:validation:XValidation:rule="has(self.target.talosVersion) || has(self.target.kubernetesVersion)",message="at least one of target.talosVersion or target.kubernetesVersion must be set"
type PatchJobSpec struct {
	// NodeName is the name of the node to patch
	// +kubebuilder:validation:Required
	NodeName string `json:"nodeName"`

	// Target defines the target Talos and/or Kubernetes version specification
	// +kubebuilder:validation:Required
	Target TargetSpec `json:"target"`

	// PatchPlanRef references the parent PatchPlan
	// +optional
	PatchPlanRef string `json:"patchPlanRef,omitempty"`

	// Group is the PatchPlan group that scheduled the node; its Lease counts against that group's
	// concurrency.
	// +optional
	Group string `json:"group,omitempty"`
}

// PatchJobStatus defines the observed state of PatchJob
type PatchJobStatus struct {
	// Phase represents the current phase of the patch operation
	// +kubebuilder:validation:Enum=Pending;Draining;Upgrading;Rebooting;UpgradingKubernetes;ValidatingKubernetes;Completed;Failed
	Phase PatchJobPhase `json:"phase,omitempty"`

	// Message contains human-readable message about current state
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when the patch operation started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the patch operation completed (success or failure)
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// CurrentTalosVersion is the Talos version before upgrade
	// +optional
	CurrentTalosVersion string `json:"currentTalosVersion,omitempty"`

	// TargetTalosVersion is the extracted Talos version tag for display (e.g., v1.11.5)
	// +optional
	TargetTalosVersion string `json:"targetTalosVersion,omitempty"`

	// CurrentKubernetesVersion is the kubelet version before upgrade, as reported by the node
	// +optional
	CurrentKubernetesVersion string `json:"currentKubernetesVersion,omitempty"`

	// TargetKubernetesVersion is the extracted Kubernetes version tag for display (e.g., v1.32.4)
	// +optional
	TargetKubernetesVersion string `json:"targetKubernetesVersion,omitempty"`

	// Conditions represent the latest available observations of the PatchJob's state
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PatchJobPhase represents the phase of a patch job
type PatchJobPhase string

const (
	PatchJobPhasePending              PatchJobPhase = "Pending"
	PatchJobPhaseDraining             PatchJobPhase = "Draining"
	PatchJobPhaseUpgrading            PatchJobPhase = "Upgrading"
	PatchJobPhaseRebooting            PatchJobPhase = "Rebooting"
	PatchJobPhaseUpgradingKubernetes  PatchJobPhase = "UpgradingKubernetes"
	PatchJobPhaseValidatingKubernetes PatchJobPhase = "ValidatingKubernetes"
	PatchJobPhaseCompleted            PatchJobPhase = "Completed"
	PatchJobPhaseFailed               PatchJobPhase = "Failed"
)

// IsTerminal returns true if the phase is terminal (Completed or Failed)
func (p PatchJobPhase) IsTerminal() bool {
	return p == PatchJobPhaseCompleted || p == PatchJobPhaseFailed
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=`.spec.nodeName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="TalosCurrent",type=string,JSONPath=`.status.currentTalosVersion`
// +kubebuilder:printcolumn:name="TalosTarget",type=string,JSONPath=`.status.targetTalosVersion`
// +kubebuilder:printcolumn:name="K8sCurrent",type=string,JSONPath=`.status.currentKubernetesVersion`
// +kubebuilder:printcolumn:name="K8sTarget",type=string,JSONPath=`.status.targetKubernetesVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PatchJob is the Schema for the patchjobs API
// PatchJobs are owned by their parent PatchPlan and will be automatically deleted
// when the PatchPlan is deleted. Finished PatchJobs are removed once the PatchPlan's
// spec.retention.history has passed.
type PatchJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PatchJobSpec   `json:"spec,omitempty"`
	Status PatchJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PatchJobList contains a list of PatchJob
type PatchJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PatchJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &PatchJob{}, &PatchJobList{})
		return nil
	})
}

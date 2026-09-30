package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TargetSpec defines the target Talos and/or Kubernetes version specification.
// At least one of TalosVersion or KubernetesVersion must be set.
type TargetSpec struct {
	// TalosVersion is the desired Talos OS version (e.g., v1.12.1). Omit to leave the Talos
	// version untouched and only upgrade KubernetesVersion.
	// +optional
	TalosVersion string `json:"talosVersion,omitempty"`

	// KubernetesVersion is the desired Kubernetes version (e.g., v1.32.4). Omit to leave the
	// Kubernetes version untouched and only upgrade TalosVersion.
	// Requires ControlPlaneFirst and PatchControlPlane to be true, since kubelets must never run
	// newer than the control plane they connect to.
	// +optional
	KubernetesVersion string `json:"kubernetesVersion,omitempty"`

	// Source specifies the image source: "factory" or "ghcr"
	// +kubebuilder:validation:Enum=factory;ghcr
	// +kubebuilder:default=ghcr
	Source string `json:"source,omitempty"`

	// Installer specifies the installer type (e.g., "aws", "azure", "nocloud")
	// Required when source=factory
	// +optional
	Installer string `json:"installer,omitempty"`

	// SchematicID is the Talos factory schematic ID
	// Required when source=factory
	// +optional
	SchematicID string `json:"schematicID,omitempty"`

	// SecureBoot enables secure boot for the installer image
	// Only applicable when source=factory
	// +kubebuilder:default=false
	// +optional
	SecureBoot bool `json:"secureBoot,omitempty"`
}

// PatchPlanSpec defines the desired state of PatchPlan
// +kubebuilder:validation:XValidation:rule="size(self.target.talosVersion) > 0 || size(self.target.kubernetesVersion) > 0",message="at least one of target.talosVersion or target.kubernetesVersion must be set"
// +kubebuilder:validation:XValidation:rule="size(self.target.kubernetesVersion) == 0 || self.controlPlaneFirst",message="controlPlaneFirst must be true when target.kubernetesVersion is set"
// +kubebuilder:validation:XValidation:rule="size(self.target.kubernetesVersion) == 0 || self.patchControlPlane",message="patchControlPlane must be true when target.kubernetesVersion is set"
type PatchPlanSpec struct {
	// Target defines the target Talos and/or Kubernetes version specification
	// +kubebuilder:validation:Required
	Target TargetSpec `json:"target"`

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

	// Cancelled permanently stops the patching operation. Unlike Paused, scheduling of new
	// nodes stops for good and the plan moves to the terminal Cancelled phase. PatchJobs
	// already in progress are not affected and run to completion.
	// +kubebuilder:default=false
	Cancelled bool `json:"cancelled,omitempty"`

	// TalosConfig contains Talos API connection information
	// +optional
	TalosConfig TalosConfig `json:"talosConfig,omitempty"`

	// Maintenance defines maintenance windows for patching operations
	// +optional
	Maintenance *MaintenanceSpec `json:"maintenance,omitempty"`
}

// MaintenanceSpec defines maintenance windows for patching operations
type MaintenanceSpec struct {
	// ExcludeDates is a list of dates in YYYY-MM-DD format (e.g., "2026-12-24") when patching is not allowed
	// +optional
	ExcludeDates []string `json:"excludeDates,omitempty"`

	// Windows defines time windows when patching is allowed
	// +optional
	Windows []MaintenanceWindow `json:"windows,omitempty"`
}

// MaintenanceWindow defines a time window for patching
type MaintenanceWindow struct {
	// Days is a list of weekdays when this window applies (e.g., ["Monday", "Friday"])
	// If empty or omitted, applies to all days
	// +optional
	Days []string `json:"days,omitempty"`

	// StartTime is the start time in HH:MM format (e.g., "01:00")
	// +kubebuilder:validation:Pattern="^([01][0-9]|2[0-3]):[0-5][0-9]$"
	StartTime string `json:"startTime"`

	// EndTime is the end time in HH:MM format (e.g., "05:00")
	// +kubebuilder:validation:Pattern="^([01][0-9]|2[0-3]):[0-5][0-9]$"
	EndTime string `json:"endTime"`

	// Disabled temporarily disables this maintenance window
	// +kubebuilder:default=false
	// +optional
	Disabled bool `json:"disabled,omitempty"`
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
	// +kubebuilder:validation:Enum=Pending;InProgress;Paused;Cancelled;Completed;Failed
	Phase PatchPhase `json:"phase,omitempty"`

	// TargetTalosVersion is the display Talos version extracted from spec.target
	// +optional
	TargetTalosVersion string `json:"targetTalosVersion,omitempty"`

	// TargetKubernetesVersion is the display Kubernetes version extracted from spec.target
	// +optional
	TargetKubernetesVersion string `json:"targetKubernetesVersion,omitempty"`

	// KubeProxyUpgraded indicates whether the cluster-wide kube-proxy DaemonSet image has been
	// rolled out to match spec.target.kubernetesVersion. Only meaningful when kubernetesVersion is
	// set; control plane nodes are upgraded before this is attempted, and worker nodes wait for it.
	// +optional
	KubeProxyUpgraded bool `json:"kubeProxyUpgraded,omitempty"`

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
	PatchPhaseCancelled  PatchPhase = "Cancelled"
	PatchPhaseCompleted  PatchPhase = "Completed"
	PatchPhaseFailed     PatchPhase = "Failed"
)

// +kubebuilder:object:root=true

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="TalosTarget",type=string,JSONPath=`.status.targetTalosVersion`
// +kubebuilder:printcolumn:name="K8sTarget",type=string,JSONPath=`.status.targetKubernetesVersion`
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
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &PatchPlan{}, &PatchPlanList{})
		return nil
	})
}

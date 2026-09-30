package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AutoUpdateSpec turns a PatchPlan into a template: instead of patching nodes itself, the plan
// watches for new Talos releases and creates a child PatchPlan with talosVersion set for each one.
type AutoUpdateSpec struct {
	// Enabled turns the PatchPlan into an auto-update template.
	Enabled bool `json:"enabled"`

	// Allow is the largest version change applied automatically, relative to the lowest Talos version
	// running on the selected nodes. "patch" only moves within the current minor (1.13.2 -> 1.13.5);
	// "minor" also allows stepping to the next minor (1.12.6 -> 1.13.x), never skipping one.
	// +kubebuilder:validation:Enum=patch;minor
	// +kubebuilder:default=patch
	// +optional
	Allow string `json:"allow,omitempty"`

	// CheckInterval is how often to look for a new release (minimum 1m).
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(s|m|h))+$"
	// +optional
	CheckInterval metav1.Duration `json:"checkInterval,omitempty"`

	// MinReleaseAge is how old a release must be before it is used, so a release that is pulled or
	// hot-fixed right after publication is never rolled out.
	// +kubebuilder:default="0s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(\\.[0-9]+)?(s|m|h))+$"
	// +optional
	MinReleaseAge metav1.Duration `json:"minReleaseAge,omitempty"`
}

// AutoUpdateStatus reports the state of the release watcher of an auto-update template.
type AutoUpdateStatus struct {
	// LastCheckTime is when the releases were last evaluated.
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`

	// CurrentVersion is the lowest Talos version found on the selected nodes.
	// +optional
	CurrentVersion string `json:"currentVersion,omitempty"`

	// LatestVersion is the newest stable upstream release, regardless of age or the allow setting.
	// +optional
	LatestVersion string `json:"latestVersion,omitempty"`

	// PendingVersion is a newer release that is still younger than minReleaseAge.
	// +optional
	PendingVersion string `json:"pendingVersion,omitempty"`

	// LastCreatedPlan is the name of the most recent child PatchPlan.
	// +optional
	LastCreatedPlan string `json:"lastCreatedPlan,omitempty"`
}

// TargetSpec defines the target Talos and/or Kubernetes version specification.
// At least one of TalosVersion, KubernetesVersion or an enabled AutoUpdate must be set.
type TargetSpec struct {
	// AutoUpdate makes this PatchPlan a template that creates a child PatchPlan per Talos release.
	// TalosVersion and KubernetesVersion must be omitted when it is enabled.
	// +optional
	AutoUpdate *AutoUpdateSpec `json:"autoUpdate,omitempty"`

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

	// SchematicID is the Talos factory schematic ID.
	// When source=factory and this is omitted, each node keeps the schematic it is currently running.
	// +optional
	SchematicID string `json:"schematicID,omitempty"`

	// SecureBoot enables secure boot for the installer image
	// Only applicable when source=factory
	// +kubebuilder:default=false
	// +optional
	SecureBoot bool `json:"secureBoot,omitempty"`
}

// PatchPlanSpec defines the desired state of PatchPlan
// +kubebuilder:validation:XValidation:rule="has(self.target.talosVersion) || has(self.target.kubernetesVersion) || (has(self.target.autoUpdate) && self.target.autoUpdate.enabled)",message="at least one of target.talosVersion, target.kubernetesVersion or an enabled target.autoUpdate must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.target.autoUpdate) && self.target.autoUpdate.enabled) || (!has(self.target.talosVersion) && !has(self.target.kubernetesVersion))",message="target.talosVersion and target.kubernetesVersion must be omitted when target.autoUpdate is enabled"
// +kubebuilder:validation:XValidation:rule="!has(self.target.kubernetesVersion) || self.controlPlaneFirst",message="controlPlaneFirst must be true when target.kubernetesVersion is set"
// +kubebuilder:validation:XValidation:rule="!has(self.target.kubernetesVersion) || self.patchControlPlane",message="patchControlPlane must be true when target.kubernetesVersion is set"
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
	// +kubebuilder:validation:Enum=Pending;Preflighting;InProgress;Paused;Cancelled;Completed;Failed;Watching
	Phase PatchPhase `json:"phase,omitempty"`

	// AutoUpdate reports the release watcher state. Only set on auto-update templates.
	// +optional
	AutoUpdate *AutoUpdateStatus `json:"autoUpdate,omitempty"`

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
	PatchPhasePending      PatchPhase = "Pending"
	PatchPhasePreflighting PatchPhase = "Preflighting"
	PatchPhaseInProgress   PatchPhase = "InProgress"
	PatchPhasePaused       PatchPhase = "Paused"
	PatchPhaseCancelled    PatchPhase = "Cancelled"
	PatchPhaseCompleted    PatchPhase = "Completed"
	PatchPhaseFailed       PatchPhase = "Failed"
	// PatchPhaseWatching is the phase of an auto-update template that is waiting for new releases.
	PatchPhaseWatching PatchPhase = "Watching"
)

// LabelParentPlan is set on child PatchPlans created by an auto-update template and holds the
// template's name.
const LabelParentPlan = "kangalpatch.ozalp.dk/parent"

// IsAutoUpdateTemplate reports whether the plan only watches for releases and creates child plans.
func (p *PatchPlan) IsAutoUpdateTemplate() bool {
	return p.Spec.Target.AutoUpdate != nil && p.Spec.Target.AutoUpdate.Enabled
}

// ConditionPreflightPassed is the PatchPlan condition type reporting the result of the checks
// that run once before the first PatchJob is created.
const ConditionPreflightPassed = "PreflightPassed"

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

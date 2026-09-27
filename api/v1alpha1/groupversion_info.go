// Package v1alpha1 contains API Schema definitions for the kangalpatch v1alpha1 API group
// +kubebuilder:object:generate=true
// +groupName=kangalpatch.ozalp.dk
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects
	GroupVersion = schema.GroupVersion{Group: "kangalpatch.ozalp.dk", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	// Required for List/Watch: registers common types (ListOptions, WatchEvent, ...) for this GroupVersion.
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}

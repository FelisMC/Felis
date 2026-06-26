// Package v1alpha1 contains the felis.lolicon.best/v1alpha1 API group, whose
// MinecraftServer kind is the lifecycle source-of-truth for Felis (spec §1, §4).
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the hardcoded software-identity API group (spec §2, §4).
	GroupName = "felis.lolicon.best"
	// Version is the API version served by this package.
	Version = "v1alpha1"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

	// SchemeBuilder registers the API types into a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&MinecraftServer{},
		&MinecraftServerList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}

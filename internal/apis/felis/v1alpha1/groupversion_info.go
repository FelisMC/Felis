// Package v1alpha1 is the felis.lolicon.best/v1alpha1 API group; MinecraftServer is the lifecycle source of truth.
// +kubebuilder:object:generate=true
// +groupName=felis.lolicon.best
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the API group every Felis object is registered under.
	GroupName = "felis.lolicon.best"
	Version   = "v1alpha1"
)

var (
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

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

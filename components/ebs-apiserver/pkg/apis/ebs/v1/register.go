package v1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "ebs"

var SchemeGroupVersion = schema.GroupVersion{
	Group:   GroupName,
	Version: "v1",
}

var (
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&Project{}, &ProjectList{},
		&Snapshot{}, &SnapshotList{},
		&Build{}, &BuildList{},
		&Job{}, &JobList{},
		&Runner{}, &RunnerList{},
	)
	return nil
}

func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

func Kind(kind string) schema.GroupKind {
	return SchemeGroupVersion.WithKind(kind).GroupKind()
}

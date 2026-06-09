package ebs

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
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

func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(SchemeGroupVersion,
		&ebsv1.Project{},
		&ebsv1.ProjectList{},
		&ebsv1.Snapshot{},
		&ebsv1.SnapshotList{},
		&ebsv1.Build{},
		&ebsv1.BuildList{},
		&ebsv1.Job{},
		&ebsv1.JobList{},
		&ebsv1.Runner{},
		&ebsv1.RunnerList{},
	)
	scheme.AddKnownTypes(schema.GroupVersion{Group: GroupName, Version: runtime.APIVersionInternal},
		&ebsv1.Project{},
		&ebsv1.ProjectList{},
		&ebsv1.Snapshot{},
		&ebsv1.SnapshotList{},
		&ebsv1.Build{},
		&ebsv1.BuildList{},
		&ebsv1.Job{},
		&ebsv1.JobList{},
		&ebsv1.Runner{},
		&ebsv1.RunnerList{},
	)
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	return nil
}

package ebs

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	ebsv1 "ebs-api/ebs/v1"
)

const GroupName = "ebs"

var SchemeGroupVersion = schema.GroupVersion{
	Group:   GroupName,
	Version: runtime.APIVersionInternal,
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
		&ebsv1.Config{}, &ebsv1.ConfigList{},
		&ebsv1.Script{}, &ebsv1.ScriptList{},
		&ebsv1.Project{}, &ebsv1.ProjectList{},
		&ebsv1.Snapshot{}, &ebsv1.SnapshotList{},
		&ebsv1.Build{}, &ebsv1.BuildList{},
		&ebsv1.BuildInfo{}, &ebsv1.BuildInfoList{},
		&ebsv1.BuildGraph{}, &ebsv1.BuildGraphList{},
		&ebsv1.RpmRepo{}, &ebsv1.RpmRepoList{},
		&ebsv1.Job{}, &ebsv1.JobList{},
		&ebsv1.Runner{}, &ebsv1.RunnerList{},
	)
	return nil
}

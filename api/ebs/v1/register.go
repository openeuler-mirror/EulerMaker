package v1

import (
	"fmt"

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
		&BuildInfo{}, &BuildInfoList{},
		&RpmRepo{}, &RpmRepoList{},
		&BuildResource{}, &BuildResourceList{},
		&Job{}, &JobList{},
		&Runner{}, &RunnerList{},
	)
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Job"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.runner", "status.phase":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for Job: %s", label)
		}
	})
	return nil
}

func Resource(resource string) schema.GroupResource {
	return SchemeGroupVersion.WithResource(resource).GroupResource()
}

func Kind(kind string) schema.GroupKind {
	return SchemeGroupVersion.WithKind(kind).GroupKind()
}

package v1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// ResourceRegistration associates an API resource path with its object and list types.
type ResourceRegistration struct {
	Resource string
	Object   runtime.Object
	List     runtime.Object
}

// ResourceRegistrations returns the resource catalog with fresh object and list prototypes.
func ResourceRegistrations() []ResourceRegistration {
	return []ResourceRegistration{
		{"configs", &Config{}, &ConfigList{}},
		{"scripts", &Script{}, &ScriptList{}},
		{"projects", &Project{}, &ProjectList{}},
		{"snapshots", &Snapshot{}, &SnapshotList{}},
		{"builds", &Build{}, &BuildList{}},
		{"buildinfos", &BuildInfo{}, &BuildInfoList{}},
		{"buildgraphs", &BuildGraph{}, &BuildGraphList{}},
		{"rpmrepos", &RpmRepo{}, &RpmRepoList{}},
		{"jobs", &Job{}, &JobList{}},
		{"oldjobs", &Job{}, &JobList{}},
		{"runners", &Runner{}, &RunnerList{}},
	}
}

func addKnownTypes(scheme *runtime.Scheme) error {
	for _, resource := range ResourceRegistrations() {
		scheme.AddKnownTypes(SchemeGroupVersion, resource.Object, resource.List)
	}
	metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Project"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.phase":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for Project: %s", label)
		}
	})
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Snapshot"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.phase":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for Snapshot: %s", label)
		}
	})
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Build"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.phase", "status.stage":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for Build: %s", label)
		}
	})
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("BuildInfo"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.phase":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for BuildInfo: %s", label)
		}
	})
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("BuildGraph"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for BuildGraph: %s", label)
		}
	})
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("RpmRepo"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.release.phase":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for RpmRepo: %s", label)
		}
	})
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Job"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.runner", "status.phase", "status.stage":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for Job: %s", label)
		}
	})
	scheme.AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Runner"), func(label, value string) (string, string, error) {
		switch label {
		case "metadata.name", "metadata.namespace", "status.phase":
			return label, value, nil
		default:
			return "", "", fmt.Errorf("field label not supported for Runner: %s", label)
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

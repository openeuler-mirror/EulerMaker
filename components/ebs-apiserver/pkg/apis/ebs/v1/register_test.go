package v1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestJobFieldLabelConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gvk := SchemeGroupVersion.WithKind("Job")
	for _, field := range []string{"metadata.name", "metadata.namespace", "status.runner", "status.phase"} {
		label, value, err := scheme.ConvertFieldLabel(gvk, field, "value")
		if err != nil || label != field || value != "value" {
			t.Fatalf("convert %q: label=%q value=%q err=%v", field, label, value, err)
		}
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "spec.runtime", "dc"); err == nil {
		t.Fatal("expected unsupported field selector error")
	}
}

func TestBuildResourceTypesAreRegistered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	for _, kind := range []string{"BuildResource", "BuildResourceList"} {
		obj, err := scheme.New(SchemeGroupVersion.WithKind(kind))
		if err != nil {
			t.Fatalf("new %s: %v", kind, err)
		}
		if obj == nil {
			t.Fatalf("new %s returned nil", kind)
		}
	}
}

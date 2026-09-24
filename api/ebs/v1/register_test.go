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

func TestBuildFieldLabelConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gvk := SchemeGroupVersion.WithKind("Build")
	for _, field := range []string{"metadata.name", "metadata.namespace", "status.phase", "status.stage"} {
		label, value, err := scheme.ConvertFieldLabel(gvk, field, "value")
		if err != nil || label != field || value != "value" {
			t.Fatalf("convert %q: label=%q value=%q err=%v", field, label, value, err)
		}
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "spec.buildTarget.os", "openEuler"); err == nil {
		t.Fatal("expected unsupported field selector error")
	}
}

func TestBuildInfoFieldLabelConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gvk := SchemeGroupVersion.WithKind("BuildInfo")
	for _, field := range []string{"metadata.name", "metadata.namespace", "status.phase"} {
		label, value, err := scheme.ConvertFieldLabel(gvk, field, "value")
		if err != nil || label != field || value != "value" {
			t.Fatalf("convert %q: label=%q value=%q err=%v", field, label, value, err)
		}
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "status.stage", "build"); err == nil {
		t.Fatal("expected unsupported field selector error")
	}
}

func TestRpmRepoFieldLabelConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gvk := SchemeGroupVersion.WithKind("RpmRepo")
	for _, field := range []string{"metadata.name", "metadata.namespace", "status.release.phase"} {
		label, value, err := scheme.ConvertFieldLabel(gvk, field, "value")
		if err != nil || label != field || value != "value" {
			t.Fatalf("convert %q: label=%q value=%q err=%v", field, label, value, err)
		}
	}
	for _, field := range []string{"status.phase", "status.repository.phase"} {
		if _, _, err := scheme.ConvertFieldLabel(gvk, field, "Ready"); err == nil {
			t.Fatalf("expected unsupported field selector error for %s", field)
		}
	}
}

func TestSnapshotFieldLabelConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gvk := SchemeGroupVersion.WithKind("Snapshot")
	for _, field := range []string{"metadata.name", "metadata.namespace", "status.phase"} {
		label, value, err := scheme.ConvertFieldLabel(gvk, field, "value")
		if err != nil || label != field || value != "value" {
			t.Fatalf("convert %q: label=%q value=%q err=%v", field, label, value, err)
		}
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "status.stage", "build"); err == nil {
		t.Fatal("expected unsupported field selector error")
	}
}

func TestRunnerFieldLabelConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gvk := SchemeGroupVersion.WithKind("Runner")
	for _, field := range []string{"metadata.name", "metadata.namespace", "status.phase"} {
		label, value, err := scheme.ConvertFieldLabel(gvk, field, "value")
		if err != nil || label != field || value != "value" {
			t.Fatalf("convert %q: label=%q value=%q err=%v", field, label, value, err)
		}
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "status.stage", "running"); err == nil {
		t.Fatal("expected unsupported field selector error")
	}
}

func TestProjectFieldLabelConversion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	gvk := SchemeGroupVersion.WithKind("Project")
	for _, field := range []string{"metadata.name", "metadata.namespace", "status.phase"} {
		label, value, err := scheme.ConvertFieldLabel(gvk, field, "value")
		if err != nil || label != field || value != "value" {
			t.Fatalf("convert %q: label=%q value=%q err=%v", field, label, value, err)
		}
	}
	if _, _, err := scheme.ConvertFieldLabel(gvk, "status.stage", "running"); err == nil {
		t.Fatal("expected unsupported field selector error")
	}
}

func TestConfigTypesAreRegistered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	for _, kind := range []string{"Config", "ConfigList"} {
		obj, err := scheme.New(SchemeGroupVersion.WithKind(kind))
		if err != nil {
			t.Fatalf("new %s: %v", kind, err)
		}
		if obj == nil {
			t.Fatalf("new %s returned nil", kind)
		}
	}
}

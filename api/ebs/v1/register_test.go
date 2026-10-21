package v1

import (
	"reflect"
	"strings"
	"testing"

	apiMeta "k8s.io/apimachinery/pkg/api/meta"
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
}

func TestResourceTypesAreRegistered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	resources := make(map[string]ResourceRegistration)
	registeredLists := make(map[string]bool)
	for _, resource := range ResourceRegistrations() {
		if _, exists := resources[resource.Resource]; exists {
			t.Fatalf("duplicate resource %q", resource.Resource)
		}
		resources[resource.Resource] = resource
		for _, prototype := range []runtime.Object{resource.Object, resource.List} {
			kinds, _, err := scheme.ObjectKinds(prototype)
			if err != nil || len(kinds) != 1 || kinds[0].GroupVersion() != SchemeGroupVersion {
				t.Fatalf("resource %s type %T: kinds=%v err=%v", resource.Resource, prototype, kinds, err)
			}
			obj, err := scheme.New(kinds[0])
			if err != nil || reflect.TypeOf(obj) != reflect.TypeOf(prototype) {
				t.Fatalf("resource %s: new %s returned %T, err=%v", resource.Resource, kinds[0], obj, err)
			}
		}
		registeredLists[reflect.TypeOf(resource.List).Elem().Name()] = true
		if err := apiMeta.SetList(resource.List, []runtime.Object{resource.Object}); err != nil {
			t.Fatalf("resource %s object/list mismatch: %v", resource.Resource, err)
		}
	}
	for kind := range scheme.KnownTypes(SchemeGroupVersion) {
		if strings.HasSuffix(kind, "List") && !registeredLists[kind] {
			t.Errorf("list %s is missing from the resource catalog", kind)
		}
	}
	for _, name := range []string{"jobs", "oldjobs"} {
		resource := resources[name]
		_, jobOK := resource.Object.(*Job)
		_, listOK := resource.List.(*JobList)
		if !jobOK || !listOK {
			t.Errorf("%s types=(%T, %T), want (*Job, *JobList)", name, resource.Object, resource.List)
		}
	}
}

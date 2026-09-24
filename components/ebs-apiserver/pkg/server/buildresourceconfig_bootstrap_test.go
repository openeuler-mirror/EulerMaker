package server

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

type fakeDefaultBuildResourceConfigStorage struct {
	object      *ebsv1.BuildResourceConfig
	getErr      error
	createErr   error
	createCalls int
}

func (f *fakeDefaultBuildResourceConfigStorage) New() runtime.Object { return &ebsv1.BuildResourceConfig{} }

func (f *fakeDefaultBuildResourceConfigStorage) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.object, nil
}

func (f *fakeDefaultBuildResourceConfigStorage) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	f.createCalls++
	f.object = obj.(*ebsv1.BuildResourceConfig)
	return obj, f.createErr
}

func TestEnsureDefaultBuildResourceConfigCreatesMissingObject(t *testing.T) {
	storage := &fakeDefaultBuildResourceConfigStorage{getErr: apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "buildresourceconfigs"}, "default")}
	if err := ensureDefaultBuildResourceConfig(context.Background(), storage); err != nil {
		t.Fatalf("ensure default: %v", err)
	}
	if storage.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", storage.createCalls)
	}
	obj := storage.object
	if obj.Name != "default" || obj.Namespace != "" || obj.Kind != "BuildResourceConfig" {
		t.Fatalf("unexpected identity: %#v", obj.ObjectMeta)
	}
	if obj.Spec.Default.Requests["cpu"] != "4" || obj.Spec.Default.Requests["memory"] != "8Gi" {
		t.Fatalf("unexpected default requests: %#v", obj.Spec.Default.Requests)
	}
	if len(obj.Spec.Packages) != 665 {
		t.Fatalf("package overrides = %d, want 665", len(obj.Spec.Packages))
	}
	atune := obj.Spec.Packages["A-Tune"].Default
	if atune.Requests["cpu"] != "8" {
		t.Fatalf("unexpected A-Tune requests: %#v", atune.Requests)
	}
	computeLibrary := obj.Spec.Packages["ComputeLibrary"].Default
	if computeLibrary.Requests["memory"] != "64Gi" {
		t.Fatalf("unexpected ComputeLibrary requests: %#v", computeLibrary.Requests)
	}
}

func TestEnsureDefaultBuildResourceConfigPreservesExistingObject(t *testing.T) {
	storage := &fakeDefaultBuildResourceConfigStorage{object: &ebsv1.BuildResourceConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}}}
	if err := ensureDefaultBuildResourceConfig(context.Background(), storage); err != nil {
		t.Fatalf("ensure default: %v", err)
	}
	if storage.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", storage.createCalls)
	}
}

func TestEnsureDefaultBuildResourceConfigAcceptsCreateRace(t *testing.T) {
	storage := &fakeDefaultBuildResourceConfigStorage{
		getErr:    apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "buildresourceconfigs"}, "default"),
		createErr: apierrors.NewAlreadyExists(schema.GroupResource{Group: "ebs", Resource: "buildresourceconfigs"}, "default"),
	}
	if err := ensureDefaultBuildResourceConfig(context.Background(), storage); err != nil {
		t.Fatalf("ensure default: %v", err)
	}
}

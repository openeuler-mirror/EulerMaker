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

type fakeDefaultBuildResourceStorage struct {
	object      *ebsv1.BuildResource
	getErr      error
	createErr   error
	createCalls int
}

func (f *fakeDefaultBuildResourceStorage) New() runtime.Object { return &ebsv1.BuildResource{} }

func (f *fakeDefaultBuildResourceStorage) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.object, nil
}

func (f *fakeDefaultBuildResourceStorage) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	f.createCalls++
	f.object = obj.(*ebsv1.BuildResource)
	return obj, f.createErr
}

func TestEnsureDefaultBuildResourceCreatesMissingObject(t *testing.T) {
	storage := &fakeDefaultBuildResourceStorage{getErr: apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "buildresources"}, "default")}
	if err := ensureDefaultBuildResource(context.Background(), storage); err != nil {
		t.Fatalf("ensure default: %v", err)
	}
	if storage.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", storage.createCalls)
	}
	obj := storage.object
	if obj.Name != "default" || obj.Namespace != "default" || obj.Kind != "BuildResource" {
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

func TestEnsureDefaultBuildResourcePreservesExistingObject(t *testing.T) {
	storage := &fakeDefaultBuildResourceStorage{object: &ebsv1.BuildResource{ObjectMeta: metav1.ObjectMeta{Name: "default"}}}
	if err := ensureDefaultBuildResource(context.Background(), storage); err != nil {
		t.Fatalf("ensure default: %v", err)
	}
	if storage.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", storage.createCalls)
	}
}

func TestEnsureDefaultBuildResourceAcceptsCreateRace(t *testing.T) {
	storage := &fakeDefaultBuildResourceStorage{
		getErr:    apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "buildresources"}, "default"),
		createErr: apierrors.NewAlreadyExists(schema.GroupResource{Group: "ebs", Resource: "buildresources"}, "default"),
	}
	if err := ensureDefaultBuildResource(context.Background(), storage); err != nil {
		t.Fatalf("ensure default: %v", err)
	}
}

package build

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

type fakeBuildStorage struct {
	build   *ebsv1.Build
	updates int
}

func (f *fakeBuildStorage) New() runtime.Object { return &ebsv1.Build{} }
func (f *fakeBuildStorage) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	return f.build.DeepCopy(), nil
}
func (f *fakeBuildStorage) Update(ctx context.Context, name string, info rest.UpdatedObjectInfo, _ rest.ValidateObjectFunc, _ rest.ValidateObjectUpdateFunc, _ bool, _ *metav1.UpdateOptions) (runtime.Object, bool, error) {
	obj, err := info.UpdatedObject(ctx, f.build.DeepCopy())
	if err != nil {
		return nil, false, err
	}
	f.build = obj.(*ebsv1.Build)
	f.updates++
	return f.build.DeepCopy(), false, nil
}

type fakeResponder struct {
	obj runtime.Object
	err error
}

func (f *fakeResponder) Object(_ int, obj runtime.Object) { f.obj = obj }
func (f *fakeResponder) Error(err error)                  { f.err = err }

func TestAbortTransitionsBuild(t *testing.T) {
	tests := []struct {
		name         string
		phase        string
		wantPhase    string
		wantUpdates  int
		wantConflict bool
	}{
		{name: "pending build", phase: "Pending", wantPhase: "Aborted", wantUpdates: 1},
		{name: "prepared build", phase: "Prepared", wantPhase: "Aborted", wantUpdates: 1},
		{name: "processing build", phase: "Processing", wantPhase: "Aborted", wantUpdates: 1},
		{name: "already aborted", phase: "Aborted", wantPhase: "Aborted"},
		{name: "obsolete aborting phase", phase: "Aborting", wantPhase: "Aborting", wantConflict: true},
		{name: "terminal build", phase: "Success", wantPhase: "Success", wantConflict: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fakeBuildStorage{build: &ebsv1.Build{
				ObjectMeta: metav1.ObjectMeta{Name: "build-a"},
				Status:     ebsv1.BuildStatus{Phase: tt.phase},
			}}
			connector := NewAbortStorage(backend, backend).(rest.Connecter)
			responder := &fakeResponder{}
			handler, err := connector.Connect(context.Background(), "build-a", nil, responder)
			if err != nil {
				t.Fatal(err)
			}
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/abort", nil))

			if tt.wantConflict {
				if !apierrors.IsConflict(responder.err) {
					t.Fatalf("expected conflict, got %v", responder.err)
				}
			} else if responder.err != nil {
				t.Fatalf("unexpected error: %v", responder.err)
			}
			if backend.build.Status.Phase != tt.wantPhase || backend.updates != tt.wantUpdates {
				t.Fatalf("phase=%s updates=%d, want phase=%s updates=%d", backend.build.Status.Phase, backend.updates, tt.wantPhase, tt.wantUpdates)
			}
		})
	}
}

func TestPrepareForCreateAddsMissingBuildTargetLabels(t *testing.T) {
	build := &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildTarget: ebsv1.BuildTarget{Os: "openEuler", Arch: "x86_64"}}}

	(&strategy{}).PrepareForCreate(context.Background(), build)

	if build.Labels[ebsv1.BuildTargetOSLabel] != "openEuler" || build.Labels[ebsv1.BuildTargetArchLabel] != "x86_64" || build.Labels[ebsv1.BuildTypeLabel] != "full" {
		t.Fatalf("labels = %#v", build.Labels)
	}
}

func TestPrepareForUpdateAddsMissingBuildTargetLabels(t *testing.T) {
	oldBuild := &ebsv1.Build{Status: ebsv1.BuildStatus{Phase: "Processing"}}
	newBuild := &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildType: "incremental", BuildTarget: ebsv1.BuildTarget{Os: "openEuler", Arch: "x86_64"}}}

	(&strategy{}).PrepareForUpdate(context.Background(), newBuild, oldBuild)

	if newBuild.Labels[ebsv1.BuildTargetOSLabel] != "openEuler" || newBuild.Labels[ebsv1.BuildTargetArchLabel] != "x86_64" || newBuild.Labels[ebsv1.BuildTypeLabel] != "incremental" {
		t.Fatalf("labels = %#v", newBuild.Labels)
	}
	if newBuild.Status.Phase != "Processing" {
		t.Fatalf("phase = %q", newBuild.Status.Phase)
	}
}

func TestPrepareForCreatePreservesConflictingBuildTargetLabels(t *testing.T) {
	build := &ebsv1.Build{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			ebsv1.BuildTargetOSLabel:   "another-os",
			ebsv1.BuildTargetArchLabel: "aarch64",
			ebsv1.BuildTypeLabel:       "incremental",
		}},
		Spec: ebsv1.BuildSpec{BuildTarget: ebsv1.BuildTarget{Os: "openEuler", Arch: "x86_64"}},
	}

	(&strategy{}).PrepareForCreate(context.Background(), build)

	if build.Labels[ebsv1.BuildTargetOSLabel] != "another-os" || build.Labels[ebsv1.BuildTargetArchLabel] != "aarch64" || build.Labels[ebsv1.BuildTypeLabel] != "incremental" {
		t.Fatalf("labels = %#v", build.Labels)
	}
}

func TestStatusPrepareForUpdatePreservesObjectMetadata(t *testing.T) {
	oldBuild := &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{
		Name:            "build-a",
		Namespace:       "project-a",
		ResourceVersion: "7",
		Labels:          map[string]string{ebsv1.BuildTargetOSLabel: "openEuler", ebsv1.BuildTargetArchLabel: "x86_64", ebsv1.BuildTypeLabel: "full"},
		Annotations:     map[string]string{"existing": "value"},
	}, Spec: ebsv1.BuildSpec{BuildType: "full"}}
	newBuild := &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{
		Name:            "build-a",
		Namespace:       "project-a",
		ResourceVersion: "7",
		Labels:          map[string]string{ebsv1.BuildTargetOSLabel: "tampered"},
		Annotations:     map[string]string{"tampered": "value"},
	}, Spec: ebsv1.BuildSpec{BuildType: "incremental"}, Status: ebsv1.BuildStatus{Phase: "Processing"}}

	(&statusStrategy{}).PrepareForUpdate(context.Background(), newBuild, oldBuild)

	if newBuild.Labels[ebsv1.BuildTargetOSLabel] != "openEuler" || newBuild.Labels[ebsv1.BuildTargetArchLabel] != "x86_64" || newBuild.Labels[ebsv1.BuildTypeLabel] != "full" {
		t.Fatalf("labels = %#v", newBuild.Labels)
	}
	if len(newBuild.Annotations) != 1 || newBuild.Annotations["existing"] != "value" {
		t.Fatalf("annotations = %#v", newBuild.Annotations)
	}
	if newBuild.Spec.BuildType != "full" || newBuild.Status.Phase != "Processing" {
		t.Fatalf("spec=%#v status=%#v", newBuild.Spec, newBuild.Status)
	}
}

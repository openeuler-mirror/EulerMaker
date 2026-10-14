package rpmrepo

import (
	"context"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"

	ebsv1 "ebs-api/ebs/v1"
)

func targetLabels() map[string]string {
	return map[string]string{
		ebsv1.BuildTargetOSLabel:   "openEuler-22.03-LTS",
		ebsv1.BuildTargetArchLabel: "aarch64",
	}
}

func TestStatusPreservesMetadataAndValidatesRelease(t *testing.T) {
	strategy := NewStorage().Status.(*genericregistry.Store).UpdateStrategy
	old := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{
		Name: "build-a", Namespace: "project-a", ResourceVersion: "7",
		Labels: targetLabels(), Annotations: map[string]string{"original": "value"},
	}}
	obj := old.DeepCopy()
	obj.ResourceVersion = "8"
	obj.Labels = map[string]string{"tampered": "value"}
	obj.Annotations = nil
	obj.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseFailed}
	obj.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionPublishSucceed, Status: metav1.ConditionFalse, Reason: ebsv1.RpmRepoReasonBuildAborted}}
	strategy.PrepareForUpdate(context.Background(), obj, old)
	if !reflect.DeepEqual(obj.Labels, old.Labels) || !reflect.DeepEqual(obj.Annotations, old.Annotations) || obj.ResourceVersion != "8" {
		t.Fatalf("metadata not protected or request resourceVersion lost: %+v", obj.ObjectMeta)
	}
	if errs := strategy.ValidateUpdate(context.Background(), obj, old); len(errs) != 0 {
		t.Fatalf("Failed with abort reason rejected: %v", errs)
	}
	obj.Status.Release.Transition = &ebsv1.ReleaseTransition{SourceRepositoryUID: "base"}
	if errs := strategy.ValidateUpdate(context.Background(), obj, old); len(errs) == 0 {
		t.Fatal("terminal release checkpoint accepted")
	}
}

func TestCreatePreservesRepositoryBaseline(t *testing.T) {
	strategy := NewStorage().Resource.(*genericregistry.Store).CreateStrategy
	for _, tc := range []struct {
		name    string
		uid     string
		url     string
		invalid bool
	}{
		{"empty", "", "", false},
		{"baseline", "version-1", "https://artifact/repositories/v1/version-1", false},
		{"uid-only", "version-1", "", true},
		{"url-only", "", "https://artifact/repositories/v1/version-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &ebsv1.RpmRepo{
				ObjectMeta: metav1.ObjectMeta{Name: "build-a", Labels: targetLabels()},
				Status: ebsv1.RpmRepoStatus{
					Repository: &ebsv1.RpmRepoRepositoryStatus{
						RepositoryUID: tc.uid, ContentURL: tc.url,
						SourceJobNames: []string{"job-1"}, Transition: &ebsv1.RepositoryTransition{},
						UpdatedAt: &metav1.Time{},
					},
					Release:    &ebsv1.RpmRepoReleaseStatus{},
					Conditions: []metav1.Condition{{Type: "Ignored"}},
				},
			}
			strategy.PrepareForCreate(context.Background(), obj)
			invalid := tc.invalid
			want := ebsv1.RpmRepoStatus{Repository: &ebsv1.RpmRepoRepositoryStatus{
				RepositoryUID: tc.uid, ContentURL: tc.url,
			}}
			if !reflect.DeepEqual(obj.Status, want) {
				t.Fatalf("status = %+v, want %+v", obj.Status, want)
			}
			if errs := strategy.Validate(context.Background(), obj); (len(errs) > 0) != invalid {
				t.Fatalf("validation errors = %v, want invalid=%v", errs, invalid)
			}
		})
	}
	t.Run("missing-repository", func(t *testing.T) {
		obj := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Labels: targetLabels()}}
		strategy.PrepareForCreate(context.Background(), obj)
		if obj.Status.Repository != nil {
			t.Fatalf("status = %+v", obj.Status)
		}
		if errs := strategy.Validate(context.Background(), obj); len(errs) != 0 {
			t.Fatalf("validation errors = %v", errs)
		}
	})
}

func TestCreateRequiresTargetLabels(t *testing.T) {
	strategy := NewStorage().Resource.(*genericregistry.Store).CreateStrategy
	for _, tc := range []struct {
		name   string
		labels map[string]string
		field  string
	}{
		{"missing-both", nil, "metadata.labels[ebs.io/target-os]"},
		{"os-only", map[string]string{ebsv1.BuildTargetOSLabel: "openEuler-22.03-LTS"}, "metadata.labels[ebs.io/target-arch]"},
		{"arch-only", map[string]string{ebsv1.BuildTargetArchLabel: "aarch64"}, "metadata.labels[ebs.io/target-os]"},
		{"empty-values", map[string]string{ebsv1.BuildTargetOSLabel: "", ebsv1.BuildTargetArchLabel: ""}, "metadata.labels[ebs.io/target-os]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Labels: tc.labels}}
			errs := strategy.Validate(context.Background(), obj)
			if len(errs) == 0 {
				t.Fatalf("expected validation errors for labels %+v", tc.labels)
			}
			if !strings.Contains(errs[0].Field, tc.field) {
				t.Fatalf("first error field = %q, want %q", errs[0].Field, tc.field)
			}
		})
	}
	t.Run("both-present", func(t *testing.T) {
		obj := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Labels: targetLabels()}}
		if errs := strategy.Validate(context.Background(), obj); len(errs) != 0 {
			t.Fatalf("validation errors = %v", errs)
		}
	})
}

func TestCreatePreservesSkippedMarkerOnly(t *testing.T) {
	strategy := NewStorage().Resource.(*genericregistry.Store).CreateStrategy
	for _, phase := range ebsv1.RpmRepoReleasePhaseValues() {
		for _, baseline := range []bool{false, true} {
			obj := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Labels: targetLabels()}}
			obj.Status.Release = &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleasePhase(phase), ContentURL: "ignored", Transition: &ebsv1.ReleaseTransition{SourceRepositoryUID: "ignored"}}
			if baseline {
				obj.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{RepositoryUID: "base", ContentURL: "/repositories/v1/base/"}
			}
			strategy.PrepareForCreate(context.Background(), obj)
			if phase == string(ebsv1.RpmRepoReleaseSkipped) {
				if !reflect.DeepEqual(obj.Status.Release, &ebsv1.RpmRepoReleaseStatus{Phase: ebsv1.RpmRepoReleaseSkipped}) {
					t.Fatalf("skip marker lost or extra status retained: %+v", obj.Status.Release)
				}
			} else if obj.Status.Release != nil {
				t.Fatalf("unexpected initial release: %+v", obj.Status.Release)
			}
			if (obj.Status.Repository != nil) != baseline {
				t.Fatal("baseline presence changed")
			}
			if baseline && (obj.Status.Repository.RepositoryUID != "base" || obj.Status.Repository.ContentURL != "/repositories/v1/base/") {
				t.Fatal("baseline changed")
			}
			if errs := strategy.Validate(context.Background(), obj); len(errs) != 0 {
				t.Fatal(errs)
			}
		}
	}
}

// The label requirement only guards creation: objects created before the labels existed must stay
// updatable and keep accepting status writes.
func TestUpdateAllowsMissingTargetLabels(t *testing.T) {
	store := NewStorage().Resource.(*genericregistry.Store)
	obj := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{Name: "build-a"}}
	old := obj.DeepCopy()
	if errs := store.UpdateStrategy.ValidateUpdate(context.Background(), obj, old); len(errs) != 0 {
		t.Fatalf("update validation errors = %v", errs)
	}
}

package project

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"

	ebsv1 "ebs-api/ebs/v1"
)

func TestPackageRefPreservedBeforeValidation(t *testing.T) {
	for _, operation := range []string{"create", "update"} {
		for _, tc := range []struct {
			name    string
			ref     ebsv1.GitRef
			branch  ebsv1.GitRef
			invalid bool
		}{
			{name: "empty ref", branch: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "release"}},
			{name: "tag default", branch: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1"}},
			{name: "default branch"},
			{name: "partial type", ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch}, invalid: true},
			{name: "partial value", ref: ebsv1.GitRef{Value: "main"}, invalid: true},
			{name: "invalid default branch", branch: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "bad..branch"}, invalid: true},
			{name: "unsupported default commit", branch: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "0123456789012345678901234567890123456789"}, invalid: true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				obj := &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}, Spec: ebsv1.ProjectSpec{
					DefaultRef:   tc.branch,
					BuildTargets: []ebsv1.BuildTarget{{Os: "openEuler", Arch: "x86_64"}},
					PackageRepos: []ebsv1.PackageRepo{{Name: "gcc", Ref: tc.ref}},
				}}
				ctx := context.Background()
				store := NewStorage().Resource.(*genericregistry.Store)
				old := obj.DeepCopy()
				old.Status.Phase = ebsv1.ProjectActive
				if operation == "create" {
					store.CreateStrategy.PrepareForCreate(ctx, obj)
				} else {
					store.UpdateStrategy.PrepareForUpdate(ctx, obj, old)
				}
				errs := store.CreateStrategy.Validate(ctx, obj)
				if operation == "update" {
					errs = store.UpdateStrategy.ValidateUpdate(ctx, obj, old)
				}
				if (len(errs) > 0) != tc.invalid {
					t.Fatalf("validation=%v, want invalid=%v", errs, tc.invalid)
				}
				if obj.Spec.PackageRepos[0].Ref != tc.ref {
					t.Fatal("package ref was changed")
				}
				if old.Spec.PackageRepos[0].Ref != tc.ref {
					t.Fatal("old object was mutated")
				}
				if obj.Status.Phase != ebsv1.ProjectActive {
					t.Fatal("unexpected status")
				}
			})
		}
	}
}

func TestStatusPreservesProjectType(t *testing.T) {
	old := &ebsv1.Project{}
	old.Labels = map[string]string{ebsv1.ProjectTypeLabel: ebsv1.ProjectTypeCommunity}
	old.Annotations = map[string]string{ebsv1.ProjectArtifactsAnnotation: "art-01"}
	candidate := old.DeepCopy()
	candidate.Labels[ebsv1.ProjectTypeLabel] = ebsv1.ProjectTypePersonal
	candidate.Annotations[ebsv1.ProjectArtifactsAnnotation] = "art-02"
	NewStorage().Status.(*genericregistry.Store).UpdateStrategy.PrepareForUpdate(context.Background(), candidate, old)
	if candidate.Labels[ebsv1.ProjectTypeLabel] != ebsv1.ProjectTypeCommunity {
		t.Fatal("status update changed project type")
	}
	if candidate.Annotations[ebsv1.ProjectArtifactsAnnotation] != "art-01" {
		t.Fatal("status update changed artifact instance")
	}
}

package rpmrepo

import (
	"context"
	"reflect"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
)

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
			obj := &ebsv1.RpmRepo{Status: ebsv1.RpmRepoStatus{
				Repository: &ebsv1.RpmRepoRepositoryStatus{
					RepositoryUID: tc.uid, ContentURL: tc.url,
					SourceJobUIDs: []string{"job-1"}, Transition: &ebsv1.RepositoryTransition{},
					UpdatedAt: &metav1.Time{},
				},
				Release:    &ebsv1.RpmRepoReleaseStatus{},
				Conditions: []metav1.Condition{{Type: "Ignored"}},
			}}
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
		obj := &ebsv1.RpmRepo{}
		strategy.PrepareForCreate(context.Background(), obj)
		if obj.Status.Repository != nil {
			t.Fatalf("status = %+v", obj.Status)
		}
		if errs := strategy.Validate(context.Background(), obj); len(errs) != 0 {
			t.Fatalf("validation errors = %v", errs)
		}
	})
}

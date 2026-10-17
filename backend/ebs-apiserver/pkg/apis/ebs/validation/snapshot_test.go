package validation

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
)

func TestSnapshotAllowsEmptyPackageRef(t *testing.T) {
	obj := &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{
		DefaultRef:   ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1"},
		PackageRepos: []ebsv1.PackageRepo{{Name: "gcc"}},
	}}
	errs := ValidateSnapshot(obj)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors=%v", errs)
	}
	if obj.Spec.PackageRepos[0].Ref != (ebsv1.GitRef{}) {
		t.Fatal("validation mutated package ref")
	}
}

func TestValidateSnapshot(t *testing.T) {
	tests := []struct {
		name       string
		snapshot   *ebsv1.Snapshot
		wantErrs   int
		wantFields map[string]field.ErrorType
	}{
		{
			name:     "valid",
			snapshot: validSnapshot(),
		},
		{
			name:     "valid with empty spec commits",
			snapshot: &ebsv1.Snapshot{},
		},
		{
			name: "valid package refs",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "openEuler-24.03-LTS"}},
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefTag, Value: "v1.0.0"}},
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "0123456789abcdef0123456789abcdef01234567"}},
			}}},
		},
		{
			name:     "allows empty ref",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{{}}}},
		},
		{
			name: "rejects unsupported ref type",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{{
				Ref: ebsv1.GitRef{Type: "PullRequest", Value: "123"},
			}}}},
			wantErrs: 1,
			wantFields: map[string]field.ErrorType{
				"spec.packageRepos[0].ref.type": field.ErrorTypeNotSupported,
			},
		},
		{
			name: "rejects unsafe ref values",
			snapshot: &ebsv1.Snapshot{Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefBranch, Value: "../main"}},
				{Ref: ebsv1.GitRef{Type: ebsv1.GitRefCommit, Value: "abc123"}},
			}}},
			wantErrs: 2,
			wantFields: map[string]field.ErrorType{
				"spec.packageRepos[0].ref.value": field.ErrorTypeInvalid,
				"spec.packageRepos[1].ref.value": field.ErrorTypeInvalid,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateSnapshot(tt.snapshot)
			assertErrorList(t, errs, tt.wantErrs, tt.wantFields)
		})
	}
}

func TestValidateSnapshotUpdate(t *testing.T) {
	errs := ValidateSnapshotUpdate(&ebsv1.Snapshot{}, validSnapshot())
	assertErrorList(t, errs, 0, nil)
}

func validSnapshot() *ebsv1.Snapshot {
	return &ebsv1.Snapshot{
		Status: ebsv1.SnapshotStatus{
			PackageRepoStatuses: map[string]ebsv1.PackageRepoStatus{
				"pkg-a": {
					CloneURL: "http://git-server:8080/git/gitee.com/src-openeuler/pkg-a.git",
					CommitID: "abc123",
				},
			},
		},
	}
}

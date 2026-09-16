package build

import (
	"context"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
)

type latestBuildLister struct {
	rest.Lister
	t     *testing.T
	phase ebsv1.BuildPhase
	empty bool
	err   error
}

func (l *latestBuildLister) List(ctx context.Context, opts *internalversion.ListOptions) (runtime.Object, error) {
	l.t.Helper()
	if ns, _ := request.NamespaceFrom(ctx); ns != "project-a" {
		l.t.Fatalf("namespace = %q", ns)
	}
	if opts.Limit != 1 || (opts.FieldSelector != nil && !opts.FieldSelector.Empty()) {
		l.t.Fatal("must query latest without phase filtering")
	}
	for _, test := range []struct {
		os, arch, typ string
		match         bool
	}{
		{"os", "arch", "full", true}, {"os", "arch", "incremental", true},
		{"os", "arch", "specified", true}, {"os", "arch", "single", false},
		{"other", "arch", "full", false}, {"os", "other", "full", false},
	} {
		got := opts.LabelSelector.Matches(labels.Set{ebsv1.BuildTargetOSLabel: test.os, ebsv1.BuildTargetArchLabel: test.arch, ebsv1.BuildTypeLabel: test.typ})
		if got != test.match {
			l.t.Fatalf("selector mismatch for %+v", test)
		}
	}
	if l.err != nil {
		return nil, l.err
	}
	list := &ebsv1.BuildList{}
	if !l.empty {
		list.Items = []ebsv1.Build{{ObjectMeta: metav1.ObjectMeta{Name: "previous"}, Status: ebsv1.BuildStatus{Phase: l.phase}}}
	}
	return list, nil
}

func TestValidateLatestBuild(t *testing.T) {
	ctx := request.WithNamespace(context.Background(), "project-a")
	build := &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildTarget: ebsv1.BuildTarget{Os: "os", Arch: "arch"}}}
	for _, phase := range []ebsv1.BuildPhase{ebsv1.BuildPending, ebsv1.BuildPrepared, ebsv1.BuildProcessing, ebsv1.BuildSuccess, ebsv1.BuildFailed, ebsv1.BuildAborted, ebsv1.BuildSkipped, "", "Unknown"} {
		t.Run(string(phase), func(t *testing.T) {
			err := validateLatestBuild(ctx, &latestBuildLister{t: t, phase: phase}, build)
			if phase.IsTerminal() {
				if err != nil {
					t.Fatal(err)
				}
			} else if !apierrors.IsConflict(err) {
				t.Fatalf("expected Conflict, got %v", err)
			}
		})
	}
	if err := validateLatestBuild(ctx, &latestBuildLister{t: t, empty: true}, build); err != nil {
		t.Fatal(err)
	}
	want := apierrors.NewServiceUnavailable("unavailable")
	if err := validateLatestBuild(ctx, &latestBuildLister{t: t, err: want}, build); err != want {
		t.Fatalf("error not preserved: %v", err)
	}
}

func TestLimitedBuild(t *testing.T) {
	for _, typ := range []string{"", "full", "incremental", "specified", "single"} {
		if got := limitedBuild(&ebsv1.Build{Spec: ebsv1.BuildSpec{BuildType: typ}}); got != (typ != "single") {
			t.Fatalf("unexpected gate for %s", typ)
		}
	}
}

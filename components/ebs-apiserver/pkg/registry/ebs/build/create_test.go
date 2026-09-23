package build

import (
	"context"
	"errors"
	"net/http"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
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

type fullBuildLister struct {
	rest.Lister
	t      *testing.T
	exists bool
	err    error
}

func (l *fullBuildLister) List(ctx context.Context, opts *internalversion.ListOptions) (runtime.Object, error) {
	l.t.Helper()
	if ns, _ := request.NamespaceFrom(ctx); ns != "project-a" {
		l.t.Fatalf("namespace = %q", ns)
	}
	if opts.Limit != 1 || !opts.FieldSelector.Matches(fields.Set{"status.phase": string(ebsv1.BuildSuccess)}) || opts.FieldSelector.Matches(fields.Set{"status.phase": string(ebsv1.BuildFailed)}) {
		l.t.Fatal("must query one successful full Build")
	}
	for _, tc := range []struct {
		os, arch, typ string
		match         bool
	}{
		{"os", "arch", "full", true}, {"os", "arch", "incremental", false},
		{"os", "other", "full", false}, {"other", "arch", "full", false},
	} {
		if got := opts.LabelSelector.Matches(labels.Set{ebsv1.BuildTargetOSLabel: tc.os, ebsv1.BuildTargetArchLabel: tc.arch, ebsv1.BuildTypeLabel: tc.typ}); got != tc.match {
			l.t.Fatalf("selector mismatch for %+v", tc)
		}
	}
	if l.err != nil {
		return nil, l.err
	}
	list := &ebsv1.BuildList{}
	if l.exists {
		list.Items = []ebsv1.Build{{Status: ebsv1.BuildStatus{Phase: ebsv1.BuildSuccess}}}
	}
	return list, nil
}

func TestValidateFullBuildBaseline(t *testing.T) {
	ctx := request.WithNamespace(context.Background(), "project-a")
	for _, typ := range []string{"incremental", "specified"} {
		build := &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildType: typ, BuildTarget: ebsv1.BuildTarget{Os: "os", Arch: "arch"}}}
		if err := validateFullBuildBaseline(ctx, &fullBuildLister{t: t, exists: true}, build); err != nil {
			t.Fatal(err)
		}
		err := validateFullBuildBaseline(ctx, &fullBuildLister{t: t}, build)
		status, ok := err.(*apierrors.StatusError)
		if !ok || status.ErrStatus.Code != http.StatusPreconditionFailed || status.ErrStatus.Reason != "FullBuildRequired" || status.ErrStatus.Message != "No complete full build exists yet for this project and target" {
			t.Fatalf("unexpected missing-baseline response: %v", err)
		}
		want := errors.New("list unavailable")
		if err := validateFullBuildBaseline(ctx, &fullBuildLister{t: t, err: want}, build); err != want {
			t.Fatalf("list error not preserved: %v", err)
		}
	}
	if err := validateFullBuildBaseline(ctx, &fullBuildLister{t: t}, &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildType: "full"}}); err != nil {
		t.Fatal(err)
	}
}

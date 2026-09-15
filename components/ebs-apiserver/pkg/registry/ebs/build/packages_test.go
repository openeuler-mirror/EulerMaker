package build

import (
	"context"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	request "k8s.io/apiserver/pkg/endpoints/request"
)

type projectGetter struct {
	t     *testing.T
	err   error
	calls int
}

func (g *projectGetter) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	g.calls++
	if ns, _ := request.NamespaceFrom(ctx); ns != "" || name != "project-a" {
		g.t.Fatalf("unexpected project GET: namespace=%q name=%q", ns, name)
	}
	return &ebsv1.Project{Spec: ebsv1.ProjectSpec{PackageRepos: []ebsv1.PackageRepo{{Name: "gcc"}, {Name: "bash"}}}}, g.err
}

func TestValidateProjectPackages(t *testing.T) {
	for _, typ := range []string{"single", "specified", "full", "incremental"} {
		for _, missing := range []bool{false, true} {
			t.Run(typ+"/"+map[bool]string{true: "missing", false: "valid"}[missing], func(t *testing.T) {
				g := &projectGetter{t: t}
				packages := []string{"gcc", "bash", "gcc"}
				if missing {
					packages = append(packages, "unknown", "")
				}
				b := &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "build-a"}, Spec: ebsv1.BuildSpec{BuildType: typ, Packages: packages}}
				err := ValidateProjectPackages(g)(request.WithNamespace(context.Background(), "project-a"), b)
				checked := typ == "single" || typ == "specified"
				if checked && missing {
					if !apierrors.IsInvalid(err) {
						t.Fatalf("expected Invalid, got %v", err)
					}
					causes := err.(apierrors.APIStatus).Status().Details.Causes
					if len(causes) != 2 || causes[0].Field != "spec.packages[3]" || causes[1].Field != "spec.packages[4]" {
						t.Fatalf("unexpected causes: %#v", causes)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				wantCalls := 0
				if checked {
					wantCalls = 1
				}
				if g.calls != wantCalls {
					t.Fatalf("GET count = %d, want %d", g.calls, wantCalls)
				}
			})
		}
	}
}

func TestProjectReadErrorsPreserved(t *testing.T) {
	for _, err := range []error{apierrors.NewNotFound(ebsv1.Resource("projects"), "project-a"), apierrors.NewServiceUnavailable("unavailable"), context.Canceled} {
		g := &projectGetter{t: t, err: err}
		b := &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildType: "single", Packages: []string{"gcc"}}}
		if got := ValidateProjectPackages(g)(request.WithNamespace(context.Background(), "project-a"), b); got != err {
			t.Fatalf("got %v, want original %v", got, err)
		}
	}
}

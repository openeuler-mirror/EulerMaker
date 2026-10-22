package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
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

func (l *fullBuildLister) Exists(ctx context.Context, opts *internalversion.ListOptions) (bool, error) {
	l.t.Helper()
	if ns, _ := request.NamespaceFrom(ctx); ns != "project-a" {
		l.t.Fatalf("namespace = %q", ns)
	}
	if !opts.FieldSelector.Matches(fields.Set{"status.phase": string(ebsv1.BuildSuccess)}) || opts.FieldSelector.Matches(fields.Set{"status.phase": string(ebsv1.BuildFailed)}) {
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
	return l.exists, l.err
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
		want := errors.New("existence query unavailable")
		if err := validateFullBuildBaseline(ctx, &fullBuildLister{t: t, err: want}, build); err != want {
			t.Fatalf("existence query error not preserved: %v", err)
		}
	}
	if err := validateFullBuildBaseline(ctx, &fullBuildLister{t: t}, &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildType: "full"}}); err != nil {
		t.Fatal(err)
	}
}

type admissionRoundTripper func(*http.Request) (*http.Response, error)

func (f admissionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestClaimManagerAttachesCreateHook(t *testing.T) {
	reads := 0
	client := es.NewClientForTesting("http://elasticsearch", &http.Client{Transport: admissionRoundTripper(
		func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet || !strings.HasPrefix(req.URL.Path, "/ebs-builds/_doc/") {
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
			}
			reads++
			return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"found":false}`))}, nil
		})})
	store := esstore.New(client, "build", "Build", NewStorage().Resource.(*genericregistry.Store))
	manager := NewClaimManager(store, newCoordinationFake())
	ctx := request.WithNamespace(context.Background(), "project-a")
	build := testBuild("01234567-89ab-4cde-8fab-0123456789ab", "single")
	build.Spec.Packages = []string{"gcc"}
	build.UID = ""
	obj, err := store.Create(ctx, build, nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || obj.(*ebsv1.Build).Status.Phase != ebsv1.BuildPending {
		t.Fatalf("reads = %d, created Build = %+v", reads, obj)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	manager.Run(ctx)
}

func TestCreateBuildGitRef(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		for _, tc := range []struct {
			name, typ, ref, packageName string
			wantField                   string
		}{
			{name: "single", typ: "single", ref: "refs/merge-requests/501/head", packageName: "gcc"},
			{name: "full", typ: "full", ref: "refs/heads/main", packageName: "gcc", wantField: "metadata.annotations[ebs.io/git-ref]"},
			{name: "incremental", typ: "incremental", ref: "refs/heads/main", packageName: "gcc", wantField: "metadata.annotations[ebs.io/git-ref]"},
			{name: "empty ref", typ: "single", packageName: "gcc", wantField: "metadata.annotations[ebs.io/git-ref]"},
			{name: "missing package", typ: "single", ref: "refs/heads/main", packageName: "unknown", wantField: "spec.packages[0]"},
		} {
			t.Run(fmt.Sprintf("%s/dryRun=%t", tc.name, dryRun), func(t *testing.T) {
				writes := 0
				client := es.NewClientForTesting("http://elasticsearch", &http.Client{Transport: admissionRoundTripper(
					func(req *http.Request) (*http.Response, error) {
						writes++
						return &http.Response{StatusCode: http.StatusCreated, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader(`{"_seq_no":0,"_primary_term":1}`))}, nil
					})})
				store := esstore.New(client, "build", "Build", NewStorage().Resource.(*genericregistry.Store))
				project := &projectGetter{t: t}
				store.SetCreateHook(ValidateProjectPackages(project))
				build := testBuild("01234567-89ab-4cde-8fab-0123456789ab", tc.typ)
				build.Spec.Packages = []string{tc.packageName}
				build.Annotations = map[string]string{ebsv1.BuildGitRefAnnotation: tc.ref}
				opts := &metav1.CreateOptions{}
				if dryRun {
					opts.DryRun = []string{metav1.DryRunAll}
				}
				obj, err := store.Create(request.WithNamespace(context.Background(), "project-a"), build, nil, opts)
				if tc.wantField != "" {
					if !apierrors.IsInvalid(err) {
						t.Fatalf("expected Invalid, got %v", err)
					}
					status := err.(apierrors.APIStatus).Status()
					if status.Code != 422 || len(status.Details.Causes) != 1 || status.Details.Causes[0].Field != tc.wantField {
						t.Fatalf("unexpected status: %+v", status)
					}
					if writes != 0 {
						t.Fatalf("invalid request persisted: %d writes", writes)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				wantWrites := 1
				if dryRun {
					wantWrites = 0
				}
				if writes != wantWrites || project.calls != 1 || obj.(*ebsv1.Build).Annotations[ebsv1.BuildGitRefAnnotation] != tc.ref {
					t.Fatalf("writes=%d project reads=%d object=%+v", writes, project.calls, obj)
				}
			})
		}
	}
}

func TestAdmissionQueriesUseMinimalSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		buildType  string
		baseline   bool
		includes   []string
		data       string
		wantReason metav1.StatusReason
	}{
		{"incremental baseline", "incremental", true, nil, "exists", ""},
		{"specified baseline", "specified", true, nil, "exists", ""},
		{"missing baseline", "incremental", true, nil, "", "FullBuildRequired"},
		{"latest terminal", "full", false,
			[]string{"data.metadata.name", "data.status.phase"},
			`{"metadata":{"name":"previous"},"status":{"phase":"Success"}}`, ""},
		{"latest active", "full", false,
			[]string{"data.metadata.name", "data.status.phase"},
			`{"metadata":{"name":"previous"},"status":{"phase":"Processing"}}`, metav1.StatusReasonConflict},
		{"no previous build", "full", false,
			[]string{"data.metadata.name", "data.status.phase"}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searches := 0
			requests := 0
			client := es.NewClientForTesting("http://elasticsearch", &http.Client{Transport: admissionRoundTripper(
				func(req *http.Request) (*http.Response, error) {
					requests++
					body := `{}`
					switch req.URL.Path {
					case "/ebs-builds/_search":
						searches++
						body = `{"hits":{"hits":[]}}`
						if tc.data != "" {
							body = `{"hits":{"hits":[{"_id":"project-a/previous"}]}}`
						}
					case "/ebs-builds/_pit":
						body = `{"id":"pit-admission"}`
					case "/_search":
						searches++
						var query struct {
							Source es.SourceFilter `json:"_source"`
							Size   int64           `json:"size"`
						}
						if err := json.NewDecoder(req.Body).Decode(&query); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(query.Source.Includes, tc.includes) || query.Size != 1 {
							t.Fatalf("query = %+v, want includes %v and size 1", query, tc.includes)
						}
						body = `{"hits":{"total":{"value":0},"hits":[]}}`
						if tc.data != "" {
							body = `{"hits":{"total":{"value":1},"hits":[{"_id":"project-a/previous",` +
								`"_seq_no":1,"_primary_term":1,"_source":{"data":` + tc.data + `}}]}}`
						}
					case "/_pit":
					default:
						t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(body))}, nil
				})})
			template := NewStorage()
			store := esstore.New(client, "build", "Build", template.Resource.(*genericregistry.Store))
			ctx := request.WithNamespace(context.Background(), "project-a")
			var err error
			if tc.baseline {
				err = validateFullBuildBaseline(ctx, store, testBuild("current", tc.buildType))
			} else {
				err = validateLatestBuild(ctx, store, testBuild("current", tc.buildType))
			}
			if tc.wantReason == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantReason != "" && apierrors.ReasonForError(err) != tc.wantReason {
				t.Fatalf("error = %v, want reason %s", err, tc.wantReason)
			}
			if searches != 1 {
				t.Fatalf("searches = %d, want 1", searches)
			}
			if tc.baseline && requests != 1 {
				t.Fatalf("baseline requests = %d, want 1", requests)
			}
		})
	}
}

func TestUnconfiguredBuildDoesNotAcquireClaim(t *testing.T) {
	for _, buildType := range []string{"single", "full", "incremental", "specified"} {
		t.Run(buildType, func(t *testing.T) {
			store := esstore.New(nil, "build", "Build", NewStorage().Resource.(*genericregistry.Store))
			calls := 0
			hook := ValidateBuildTargetConfig(confGetter{obj: configForTest(ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{}}), t: t}, nil)
			store.SetCreateHook(func(ctx context.Context, obj runtime.Object) error { calls++; return hook(ctx, obj) })
			store.SetCreateTransaction(func(context.Context, runtime.Object, bool, func() (runtime.Object, error)) (runtime.Object, error) {
				t.Fatal("invalid target acquired claim")
				return nil, nil
			})
			obj := &ebsv1.Build{ObjectMeta: metav1.ObjectMeta{Name: "01234567-89ab-4cde-8fab-0123456789ab", Namespace: "project"}, Spec: ebsv1.BuildSpec{BuildType: buildType, Packages: []string{"gcc"}, BuildTarget: ebsv1.BuildTarget{Os: "openEuler", Arch: "x86_64"}}}
			_, err := store.Create(request.WithNamespace(context.Background(), "project"), obj, nil, &metav1.CreateOptions{})
			if !apierrors.IsInvalid(err) || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

type confGetter struct {
	obj runtime.Object
	err error
	t   *testing.T
}

func (g confGetter) New() runtime.Object { return &ebsv1.Config{} }
func (g confGetter) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if request.NamespaceValue(ctx) != "" || name != ebsv1.BuildTargetConfigName {
		g.t.Fatal("Config must be read cluster-wide")
	}
	return g.obj, g.err
}

func TestBuildTargetConfigCreateHook(t *testing.T) {
	conf := configForTest(ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{"os": {Arches: map[string]ebsv1.BuildTargetArch{"arch": {Image: "build:v1"}}}}})
	for _, tc := range []struct {
		name string
		obj  runtime.Object
		err  error
		arch string
		code int
	}{
		{"allowed", conf, nil, "arch", 0},
		{"unsupported", conf, nil, "other", 422},
		{"empty", configForTest(ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{}}), nil, "arch", 422},
		{"unavailable", nil, errors.New("unavailable"), "arch", 503},
		{"missing", nil, apierrors.NewNotFound(ebsv1.Resource("configs"), ebsv1.BuildTargetConfigName), "arch", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			hook := ValidateBuildTargetConfig(confGetter{tc.obj, tc.err, t}, func(context.Context, runtime.Object) error { calls++; return nil })
			err := hook(request.WithNamespace(context.Background(), "project"), &ebsv1.Build{Spec: ebsv1.BuildSpec{BuildTarget: ebsv1.BuildTarget{Os: "os", Arch: tc.arch}}})
			if tc.code == 0 {
				if err != nil || calls != 1 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
				return
			}
			status, ok := err.(apierrors.APIStatus)
			if !ok || int(status.Status().Code) != tc.code || calls != 0 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func configForTest(spec ebsv1.BuildTargetContent) *ebsv1.Config {
	content, _ := json.Marshal(spec)
	return &ebsv1.Config{ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildTargetConfigName}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityPublic, Content: string(content)}}
}

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

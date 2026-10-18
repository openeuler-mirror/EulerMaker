package build

import (
	"context"
	"encoding/json"
	"errors"
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

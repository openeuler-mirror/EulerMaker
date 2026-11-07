package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emicklei/go-restful/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apiserver/pkg/endpoints"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	esclient "ebs-apiserver/pkg/storage/es"
)

func TestOldJobsHTTPRoutes(t *testing.T) {
	deletes := 0
	client := esclient.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ebs-oldjobs/_doc/project/job" {
			t.Fatalf("ES path=%s", r.URL.Path)
		}
		if r.Method == http.MethodPut {
			deletes++
			q := r.URL.Query()
			if q.Get("if_seq_no") != "99" || q.Get("if_primary_term") != "1" || q.Get("refresh") != "wait_for" {
				t.Fatalf("delete query=%s", r.URL.RawQuery)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"_seq_no":100,"_primary_term":1}`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"_id":"project/job","_seq_no":99,"_primary_term":1,"_source":{"archive":{"uid":"uid","digest":"snapshot-digest","action":"retain"},"data":{"apiVersion":"ebs/v1","kind":"Job","metadata":{"name":"job","namespace":"project","uid":"uid","resourceVersion":"123","labels":{"ebs.io/build-name":"build"}},"spec":{"priority":1},"status":{"phase":"Succeeded"}}}}`))}, nil
	})})
	info, _, err := CreateAPIGroupInfo(testRESTOptions(), client)
	if err != nil {
		t.Fatal(err)
	}
	storage := info.VersionedResourcesStorageMap["v1"]
	container := restful.NewContainer()
	group := &endpoints.APIGroupVersion{
		Storage: map[string]rest.Storage{
			"oldjobs": storage["oldjobs"], "jobs": storage["jobs"], "jobs/archive": storage["jobs/archive"],
			"jobs/finalizers": storage["jobs/finalizers"], "oldjobs/finalizers": storage["oldjobs/finalizers"],
		},
		Root: "/apis", GroupVersion: ebsv1.SchemeGroupVersion, OptionsExternalVersion: &schema.GroupVersion{Version: "v1"},
		Creater: Scheme, Convertor: Scheme, UnsafeConvertor: runtime.UnsafeObjectConvertor(Scheme),
		Defaulter: Scheme, Typer: Scheme, Namer: meta.NewAccessor(),
		TypeConverter:              managedfields.NewDeducedTypeConverter(),
		EquivalentResourceRegistry: runtime.NewEquivalentResourceRegistry(),
		ParameterCodec:             metav1.ParameterCodec, Serializer: Codecs,
	}
	if _, _, err := group.InstallREST(container); err != nil {
		t.Fatal(err)
	}
	methods := map[string][]string{}
	for _, service := range container.RegisteredWebServices() {
		for _, route := range service.Routes() {
			methods[route.Path] = append(methods[route.Path], route.Method)
		}
	}
	for _, path := range []string{"/apis/ebs/v1/oldjobs", "/apis/ebs/v1/namespaces/{namespace}/oldjobs"} {
		if strings.Join(methods[path], ",") != "GET" {
			t.Fatalf("route %s methods=%v", path, methods[path])
		}
	}
	namedMethods := methods["/apis/ebs/v1/namespaces/{namespace}/oldjobs/{name}"]
	if len(namedMethods) != 2 || !strings.Contains(strings.Join(namedMethods, ","), "GET") ||
		!strings.Contains(strings.Join(namedMethods, ","), "DELETE") {
		t.Fatalf("named OldJob methods=%v", namedMethods)
	}
	if strings.Join(methods["/apis/ebs/v1/namespaces/{namespace}/jobs/{name}/archive"], ",") != "GET,POST" {
		t.Fatalf("archive route=%v", methods)
	}
	for _, resource := range []string{"jobs", "oldjobs"} {
		path := "/apis/ebs/v1/namespaces/{namespace}/" + resource + "/{name}/finalizers"
		if strings.Join(methods[path], ",") != "PUT" {
			t.Fatalf("finalizer route %s: %v", path, methods[path])
		}
	}
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"?includeFields=metadata,status.phase", 200},
		{"?watch=true", 400}, {"?resourceVersion=123", 400}, {"?resourceVersionMatch=Exact", 400},
	} {
		r := httptest.NewRequest("GET", "/apis/ebs/v1/namespaces/project/oldjobs/job"+tc.query, nil)
		ctx := genericapirequest.WithRequestInfo(r.Context(), &genericapirequest.RequestInfo{IsResourceRequest: true, APIGroup: "ebs", APIVersion: "v1", Resource: "oldjobs", Namespace: "project", Name: "job", Verb: "get"})
		r = r.WithContext(genericapirequest.WithNamespace(ctx, "project"))
		w := httptest.NewRecorder()
		withOldJobReadOptions(withFieldProjection(container)).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("query=%s status=%d body=%s", tc.query, w.Code, w.Body)
		}
		if tc.status == 200 && (!strings.Contains(w.Body.String(), `"resourceVersion":"v1:99:1"`) || strings.Contains(w.Body.String(), `"spec"`)) {
			t.Fatalf("projected Job=%s", w.Body)
		}
	}
	for _, tc := range []struct {
		body    string
		status  int
		deletes int
	}{
		{`{"preconditions":{"uid":"uid","resourceVersion":"v1:99:1"},"dryRun":["All"]}`, 200, 0},
		{`{}`, 200, 1},
		{`{"preconditions":{"uid":"uid","resourceVersion":"v1:99:1"}}`, 200, 2},
	} {
		r := httptest.NewRequest(http.MethodDelete, "/apis/ebs/v1/namespaces/project/oldjobs/job", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "application/json")
		ctx := genericapirequest.WithRequestInfo(r.Context(), &genericapirequest.RequestInfo{
			IsResourceRequest: true, APIGroup: "ebs", APIVersion: "v1", Resource: "oldjobs",
			Namespace: "project", Name: "job", Verb: "delete",
		})
		r = r.WithContext(genericapirequest.WithNamespace(ctx, "project"))
		w := httptest.NewRecorder()
		withOldJobReadOptions(withFieldProjection(container)).ServeHTTP(w, r)
		if w.Code != tc.status || deletes != tc.deletes {
			t.Fatalf("DELETE status=%d deletes=%d body=%s", w.Code, deletes, w.Body)
		}
	}
}

func TestGlobalJobRequestContextRemovesAliasNameScope(t *testing.T) {
	original := &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		Path:              "/apis/ebs/v1/runners/runner-a/jobs",
		Verb:              "watch",
		APIPrefix:         "apis",
		APIGroup:          "ebs",
		APIVersion:        "v1",
		Resource:          "runners",
		Name:              "runner-a",
		Subresource:       "jobs",
	}
	ctx := genericapirequest.WithRequestInfo(context.Background(), original)
	ctx = globalJobRequestContext(ctx, "/apis/ebs/v1/jobs")

	got, ok := genericapirequest.RequestInfoFrom(ctx)
	if !ok || got == nil {
		t.Fatal("rewritten request info is missing")
	}
	if got.Path != "/apis/ebs/v1/jobs" || got.Resource != "jobs" || got.Name != "" || got.Subresource != "" {
		t.Fatalf("unexpected rewritten request info: %#v", got)
	}
	if got.Verb != "watch" || got.APIGroup != "ebs" || got.APIVersion != "v1" {
		t.Fatalf("rewritten request info lost API identity: %#v", got)
	}
	if original.Resource != "runners" || original.Name != "runner-a" || original.Subresource != "jobs" {
		t.Fatalf("original request info was mutated: %#v", original)
	}
}

func TestRunnerJobWatchIsLongRunning(t *testing.T) {
	check := runnerJobLongRunningCheck(nil)
	info := &genericapirequest.RequestInfo{Verb: "get", IsResourceRequest: true, Resource: "runners", Name: "runner-a", Subresource: "jobs"}
	tests := []struct {
		path string
		want bool
	}{
		{"/apis/ebs/v1/runners/runner-a/jobs?watch=true", true},
		{"/apis/ebs/v1/runners/runner-a/jobs?watch=1", true},
		{"/apis/ebs/v1/runners/runner-a/jobs?watch=false", false},
		{"/apis/ebs/v1/runners/runner-a/jobs", false},
		{"/apis/ebs/v1/runners/runner-a/status?watch=true", false},
	}
	for _, test := range tests {
		req := httptest.NewRequest("GET", test.path, nil)
		if got := check(req, info); got != test.want {
			t.Errorf("check(%q)=%v want %v", test.path, got, test.want)
		}
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/storage/es"
	"github.com/emicklei/go-restful/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
)

type confTransport func(*http.Request) (*http.Response, error)

func (f confTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise routing, validation, resourceVersion and generation through the real
// ES store. The transport replaces only Elasticsearch.
func TestBuildConfHTTP(t *testing.T) {
	var document json.RawMessage
	var seq int64
	writes := 0
	client := es.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		code := 200
		var body any
		if r.URL.Path == "/ebs-buildconfs/_pit" || r.URL.Path == "/_pit" || r.URL.Path == "/_search" {
			body = map[string]any{"id": "pit"}
			if r.URL.Path == "/_search" {
				body = map[string]any{"hits": map[string]any{"total": map[string]any{"value": 1}, "hits": []any{map[string]any{"_id": "default", "_seq_no": seq, "_primary_term": 1, "_source": document}}}}
			}
			data, _ := json.Marshal(body)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
		}
		if r.URL.Path != "/ebs-buildconfs/_doc/default" {
			t.Fatalf("unexpected ES path %s", r.URL.Path)
		}
		switch r.Method {
		case "GET":
			if document == nil {
				code = 404
				body = map[string]any{"found": false}
			} else {
				body = map[string]any{"_id": "default", "_seq_no": seq, "_primary_term": 1, "_source": document}
			}
		case "PUT":
			if r.URL.Query().Get("op_type") == "create" && document != nil {
				code = 409
				body = map[string]any{"error": "exists"}
				break
			}
			document, _ = io.ReadAll(r.Body)
			writes++
			seq++
			body = map[string]any{"_seq_no": seq, "_primary_term": 1}
		default:
			t.Fatalf("unexpected ES method %s", r.Method)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})})
	container := restful.NewContainer()
	container.Add(new(restful.WebService).Path("/apis/ebs/v1").Produces(restful.MIME_JSON))
	if err := installBuildConfRoutes(container, newBuildConfStore(client)); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body, content string, want int) *ebsv1.BuildConf {
		t.Helper()
		req := httptest.NewRequest(method, "/apis/ebs/v1"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", content)
		rec := httptest.NewRecorder()
		container.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, rec.Code, want, rec.Body.String())
		}
		out := new(ebsv1.BuildConf)
		if want < 300 && method != "HEAD" {
			if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	call("GET", "/buildconfs/default", "", "application/json", 404)
	obj := call("POST", "/buildconfs", `{"apiVersion":"ebs/v1","kind":"BuildConf","metadata":{"name":"default"},"spec":{"targets":{}}}`, "application/json", 201)
	if obj.ResourceVersion == "" || obj.Generation != 1 || obj.UID == "" {
		t.Fatalf("missing metadata %+v", obj.ObjectMeta)
	}
	original := obj.DeepCopy()
	listResponse := httptest.NewRecorder()
	container.ServeHTTP(listResponse, httptest.NewRequest("GET", "/apis/ebs/v1/buildconfs?limit=100", nil))
	var list ebsv1.BuildConfList
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil || listResponse.Code != 200 || list.Kind != "BuildConfList" || len(list.Items) != 1 || list.Items[0].Name != "default" {
		t.Fatalf("invalid list: %s, error=%v", listResponse.Body.String(), err)
	}
	call("POST", "/buildconfs", `{"metadata":{"name":"default"},"spec":{"targets":{}}}`, "application/json", 409)
	call("PUT", "/buildconfs/default", `{"metadata":{"name":"default"},"spec":{"targets":{}}}`, "application/json", 409)
	obj.Spec.Targets["os"] = ebsv1.BuildConfTarget{Arches: map[string]ebsv1.BuildConfArch{"arch": {Image: "build:v1"}}}
	data, _ := json.Marshal(obj)
	obj = call("PUT", "/buildconfs/default", string(data), "application/json", 200)
	if obj.Generation != 2 {
		t.Fatalf("generation=%d", obj.Generation)
	}
	data, _ = json.Marshal(original)
	call("PUT", "/buildconfs/default", string(data), "application/json", 409)
	obj = call("PATCH", "/buildconfs/default", `{"spec":{"targets":{"os":{"arches":{"arch":{"image":"build:v2"}}}}}}`, "application/merge-patch+json", 200)
	if obj.Spec.Targets["os"].Arches["arch"].Image != "build:v2" {
		t.Fatal("merge patch not applied")
	}
	call("PATCH", "/buildconfs/default", `[{"op":"replace","path":"/spec/targets/os/arches/arch/image","value":"build:v3"}]`, "application/json-patch+json", 200)
	call("PATCH", "/buildconfs/default", `{"metadata":{"resourceVersion":"stale"}}`, "application/merge-patch+json", 409)
	call("PATCH", "/buildconfs/default", "{}", "application/strategic-merge-patch+json", 400)
	call("PATCH", "/buildconfs/default", `{"spec":{"targets":{"os":{"arches":{"arch":{"image":"https://bad"}}}}}}`, "application/merge-patch+json", 422)
	call("HEAD", "/buildconfs/default", "", "application/json", 200)
	call("GET", "/buildconfs?watch=true", "", "application/json", 400)
	call("GET", "/buildconfs?fieldSelector=status.phase=Active", "", "application/json", 400)
	call("GET", "/buildconfs/other", "", "application/json", 404)
	for _, body := range []string{`{"spec":{"unknown":true}}`, `{"status":{}}`, `{"metadata":{"namespace":"project"}}`, `{"kind":"Project"}`, `{} {}`} {
		call("POST", "/buildconfs", body, "application/json", 400)
	}
	if writes != 4 {
		t.Fatalf("rejected requests persisted data: writes=%d", writes)
	}
	for _, route := range container.RegisteredWebServices()[0].Routes() {
		if route.Method == "DELETE" || strings.Contains(route.Path, "/status") || strings.Contains(route.Path, "/projects/") {
			t.Fatalf("unexpected route %+v", route)
		}
	}
}

type bootstrapConfStorage struct {
	object                *ebsv1.BuildConf
	getErr, errorOnCreate error
	race                  bool
	creates               int
}

func (f *bootstrapConfStorage) New() runtime.Object { return &ebsv1.BuildConf{} }
func (f *bootstrapConfStorage) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.object == nil {
		return nil, apierrors.NewNotFound(ebsv1.Resource("buildconfs"), "default")
	}
	return f.object, nil
}
func (f *bootstrapConfStorage) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	f.creates++
	if f.errorOnCreate != nil {
		return nil, f.errorOnCreate
	}
	f.object = obj.(*ebsv1.BuildConf)
	if f.race {
		return nil, apierrors.NewAlreadyExists(ebsv1.Resource("buildconfs"), "default")
	}
	return f.object, nil
}
func TestBootstrapBuildConf(t *testing.T) {
	for _, race := range []bool{false, true} {
		f := &bootstrapConfStorage{race: race}
		if err := ensureDefaultBuildConf(context.Background(), f); err != nil {
			t.Fatal(err)
		}
		if f.object.Name != "default" || len(f.object.Spec.Targets) != 12 {
			t.Fatalf("invalid template %+v", f.object)
		}
		if errs := validation.ValidateBuildConf(f.object); len(errs) != 0 {
			t.Fatalf("invalid default configuration: %v", errs)
		}
		images := 0
		for _, target := range f.object.Spec.Targets {
			images += len(target.Arches)
			for _, arch := range target.Arches {
				if !strings.HasPrefix(arch.Image, "swr.cn-north-4.myhuaweicloud.com/eulermaker/") {
					t.Fatalf("missing image registry prefix: %s", arch.Image)
				}
			}
		}
		if images != 53 {
			t.Fatalf("image mappings=%d, want 53", images)
		}
		if image := f.object.Spec.Targets["openEuler-24.03-LTS-SP4"].Arches["aarch64"].Image; image != "swr.cn-north-4.myhuaweicloud.com/eulermaker/openeuler:24.03-lts-sp4-arm64" {
			t.Fatalf("unexpected image: %s", image)
		}
		f.object.Spec.Targets = map[string]ebsv1.BuildConfTarget{"os": {Arches: map[string]ebsv1.BuildConfArch{"arch": {Image: "keep:v1"}}}}
		if err := ensureDefaultBuildConf(context.Background(), f); err != nil || f.creates != 1 || len(f.object.Spec.Targets) != 1 {
			t.Fatalf("overwrote existing config err=%v", err)
		}
	}
	for _, f := range []*bootstrapConfStorage{{getErr: errors.New("unavailable")}, {errorOnCreate: errors.New("invalid")}} {
		if err := ensureDefaultBuildConf(context.Background(), f); err == nil {
			t.Fatal("bootstrap ignored storage failure")
		}
	}
}

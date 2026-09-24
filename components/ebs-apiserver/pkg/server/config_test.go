package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emicklei/go-restful/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
)

type confTransport func(*http.Request) (*http.Response, error)

func (f confTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConfigHTTPCreateUpdateAndRead(t *testing.T) {
	var document json.RawMessage
	var seq int64
	client := es.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		var body any
		if r.URL.Path != "/ebs-configs/_doc/build-target" {
			t.Fatalf("unexpected ES path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			if document == nil {
				status = http.StatusNotFound
				body = map[string]any{"found": false}
			} else {
				body = map[string]any{"_id": "build-target", "_seq_no": seq, "_primary_term": 1, "_source": document}
			}
		case http.MethodPut:
			if r.URL.Query().Get("op_type") == "create" && document != nil {
				status = http.StatusConflict
				body = map[string]any{"error": "exists"}
				break
			}
			document, _ = io.ReadAll(r.Body)
			seq++
			body = map[string]any{"_seq_no": seq, "_primary_term": 1}
		default:
			t.Fatalf("unexpected ES method %s", r.Method)
		}
		data, _ := json.Marshal(body)
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
	})})
	container := restful.NewContainer()
	container.Add(new(restful.WebService).Path("/apis/ebs/v1").Produces(restful.MIME_JSON))
	if err := installConfigRoutes(container, newConfigStore(client)); err != nil {
		t.Fatal(err)
	}
	call := func(method, body string, want int) ebsv1.Config {
		req := httptest.NewRequest(method, "/apis/ebs/v1/configs/build-target", strings.NewReader(body))
		if method == http.MethodPost {
			req.URL.Path = "/apis/ebs/v1/configs"
		}
		rec := httptest.NewRecorder()
		container.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("%s: status=%d want=%d body=%s", method, rec.Code, want, rec.Body.String())
		}
		var obj ebsv1.Config
		if want < 300 && method != http.MethodHead {
			if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
				t.Fatal(err)
			}
		}
		return obj
	}
	call(http.MethodGet, "", http.StatusNotFound)
	obj := call(http.MethodPost, `{"apiVersion":"ebs/v1","kind":"Config","metadata":{"name":"build-target"},"spec":{"visibility":"Public","content":"targets: {}\n"}}`, http.StatusCreated)
	if obj.UID == "" || obj.ResourceVersion == "" || obj.Generation != 1 {
		t.Fatalf("missing metadata: %+v", obj.ObjectMeta)
	}
	call(http.MethodHead, "", http.StatusOK)
	call(http.MethodPut, `{"metadata":{"name":"build-target"},"spec":{"visibility":"Public","content":"targets: {}\n"}}`, http.StatusConflict)
	obj.Spec.Content = "targets: {other: {arches: {x86_64: {image: build:v1}}}}\n"
	data, _ := json.Marshal(obj)
	updated := call(http.MethodPut, string(data), http.StatusOK)
	if updated.Generation != 2 {
		t.Fatalf("generation=%d", updated.Generation)
	}
	call(http.MethodGet, "", http.StatusOK)
}

type configBootstrapStore struct {
	objects map[string]*ebsv1.Config
	creates int
}

func (s *configBootstrapStore) New() runtime.Object { return &ebsv1.Config{} }
func (s *configBootstrapStore) Get(_ context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	if obj := s.objects[name]; obj != nil {
		return obj.DeepCopy(), nil
	}
	return nil, apierrors.NewNotFound(ebsv1.Resource("configs"), name)
}
func (s *configBootstrapStore) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	config := obj.(*ebsv1.Config)
	if s.objects[config.Name] != nil {
		return nil, apierrors.NewAlreadyExists(ebsv1.Resource("configs"), config.Name)
	}
	s.objects[config.Name] = config.DeepCopy()
	s.creates++
	return config, nil
}

func TestEnsureDefaultConfigs(t *testing.T) {
	store := &configBootstrapStore{objects: make(map[string]*ebsv1.Config)}
	if err := ensureDefaultConfigs(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if store.creates != 2 {
		t.Fatalf("created %d configs, want 2", store.creates)
	}
	target := store.objects[ebsv1.BuildTargetConfigName]
	resource := store.objects[ebsv1.BuildResourceConfigName]
	if target.Spec.Visibility != ebsv1.ConfigVisibilityPublic || resource.Spec.Visibility != ebsv1.ConfigVisibilityOpsOnly {
		t.Fatal("incorrect default visibility")
	}
	var targets ebsv1.BuildConfSpec
	if err := yaml.UnmarshalStrict([]byte(target.Spec.Content), &targets); err != nil || len(targets.Targets) == 0 {
		t.Fatalf("target content: %v", err)
	}
	var resources ebsv1.BuildResourceConfigSpec
	if err := yaml.UnmarshalStrict([]byte(resource.Spec.Content), &resources); err != nil {
		t.Fatal(err)
	}
	if resources.Default.Requests["cpu"] != "4" || resources.Default.Requests["memory"] != "8Gi" {
		t.Fatalf("default resources: %+v", resources.Default)
	}
	target.Spec.Content = "operator change"
	if err := ensureDefaultConfigs(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if store.creates != 2 || store.objects[ebsv1.BuildTargetConfigName].Spec.Content != "operator change" {
		t.Fatal("existing Config was overwritten")
	}
}

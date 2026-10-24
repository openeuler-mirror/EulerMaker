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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/sets"
	genericapi "k8s.io/apiserver/pkg/endpoints"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/storage/es"
)

type confTransport func(*http.Request) (*http.Response, error)

func (f confTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func configScriptTestHandler(storage map[string]rest.Storage) (http.Handler, error) {
	container := restful.NewContainer()
	container.Router(restful.CurlyRouter{})
	group := &genericapi.APIGroupVersion{
		Storage:                    storage,
		Root:                       "/apis",
		GroupVersion:               ebsv1.SchemeGroupVersion,
		OptionsExternalVersion:     &schema.GroupVersion{Version: "v1"},
		Serializer:                 Codecs,
		ParameterCodec:             metav1.ParameterCodec,
		Creater:                    Scheme,
		Convertor:                  Scheme,
		TypeConverter:              managedfields.NewDeducedTypeConverter(),
		UnsafeConvertor:            runtime.UnsafeObjectConvertor(Scheme),
		Defaulter:                  Scheme,
		Typer:                      Scheme,
		Namer:                      runtime.Namer(meta.NewAccessor()),
		EquivalentResourceRegistry: runtime.NewEquivalentResourceRegistry(),
		MaxRequestBodyBytes:        3 << 20,
	}
	if _, _, err := group.InstallREST(container); err != nil {
		return nil, err
	}
	resolver := &apirequest.RequestInfoFactory{APIPrefixes: sets.NewString("apis")}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info, err := resolver.NewRequestInfo(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		container.ServeHTTP(w, r.WithContext(apirequest.WithRequestInfo(r.Context(), info)))
	}), nil
}

func TestConfigHTTPCreateUpdateAndRead(t *testing.T) {
	for _, tc := range []struct{ name, initial, updated string }{
		{"build-target", "targets: {}", "targets: {other: {arches: {x86_64: {image: build:v1}}}}"},
		{"build-resource", "default: {requests: {cpu: '4', memory: 8Gi}}", "default: {requests: {cpu: '8', memory: 16Gi}}"},
		{
			"services",
			"artifactManagers: {default: {internalURL: 'http://artifact-manager:8081'}}\n" +
				"gitServer: {internalURL: 'http://git-server:8080', runnerInternalURL: 'http://127.0.0.1:8084'}",
			"artifactManagers: {art-01: {internalURL: 'http://artifact-manager.art-01:8081'}}\n" +
				"gitServer: {internalURL: 'https://git-server:8443/git-prefix', runnerInternalURL: 'http://git.internal:8084', runnerExternalURL: 'https://git.example.com'}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) { testConfigHTTPCreateUpdateAndRead(t, tc.name, tc.initial, tc.updated) })
	}
}

func testConfigHTTPCreateUpdateAndRead(t *testing.T, name, initialContent, updatedContent string) {
	var document json.RawMessage
	var seq int64
	client := es.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		var body any
		if r.URL.Path != "/ebs-configs/_doc/"+name {
			t.Fatalf("unexpected ES path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			if document == nil {
				status = http.StatusNotFound
				body = map[string]any{"found": false}
			} else {
				body = map[string]any{"_id": name, "_seq_no": seq, "_primary_term": 1, "_source": document}
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
	handler, err := configScriptTestHandler(map[string]rest.Storage{"configs": newConfigStore(client)})
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, body string, want int) ebsv1.Config {
		req := httptest.NewRequest(method, "/apis/ebs/v1/configs/"+name, strings.NewReader(body))
		if method == http.MethodPost {
			req.URL.Path = "/apis/ebs/v1/configs"
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
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
	candidate := ebsv1.Config{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Config"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityPublic, Content: initialContent},
	}
	if name == ebsv1.ServiceConfigName {
		candidate.Spec.Visibility = ebsv1.ConfigVisibilityOpsOnly
	}
	encode := func(obj ebsv1.Config) string {
		data, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	invalid := candidate
	if name == ebsv1.ServiceConfigName {
		invalid.Spec.Visibility = ebsv1.ConfigVisibilityPublic
		call(http.MethodPost, encode(invalid), http.StatusUnprocessableEntity)
		invalid = candidate
	}
	invalid.Spec.Content = "{}"
	call(http.MethodPost, encode(invalid), http.StatusUnprocessableEntity)
	obj := call(http.MethodPost, encode(candidate), http.StatusCreated)
	if obj.UID == "" || obj.ResourceVersion == "" || obj.Generation != 1 {
		t.Fatalf("missing metadata: %+v", obj.ObjectMeta)
	}
	call(http.MethodHead, "", http.StatusMethodNotAllowed)
	call(http.MethodPut, encode(candidate), http.StatusConflict)
	invalid = obj
	if name == ebsv1.ServiceConfigName {
		invalid.Spec.Visibility = ebsv1.ConfigVisibilityPublic
		call(http.MethodPut, encode(invalid), http.StatusUnprocessableEntity)
		invalid = obj
		invalid.Spec.Content = "artifactManagers: {default: {internalURL: 'http://artifact-manager:8081'}}"
		call(http.MethodPut, encode(invalid), http.StatusUnprocessableEntity)
		invalid.Spec.Content += "\ngitServer: {internalURL: 'file:///tmp/git'}"
		call(http.MethodPut, encode(invalid), http.StatusUnprocessableEntity)
		invalid = obj
	}
	if name == ebsv1.BuildResourceConfigName {
		invalid.Spec.Content = "default: {requests: {cpu: '4', memory: 8Gi}, limits: {cpu: '2'}}"
	} else if name == ebsv1.BuildTargetConfigName {
		invalid.Spec.Content = "targets: {os: {arches: {x86_64: {image: 'https://invalid/image'}}}}"
	} else {
		invalid.Spec.Content = "artifactManagers: {default: {internalURL: 'http://artifact-manager:8081', runnerExternalURL: 'ftp://external'}}\ngitServer: {internalURL: 'http://git-server:8080'}"
	}
	call(http.MethodPut, encode(invalid), http.StatusUnprocessableEntity)
	unchanged := call(http.MethodGet, "", http.StatusOK)
	if unchanged.ResourceVersion != obj.ResourceVersion || unchanged.Spec.Content != initialContent {
		t.Fatal("rejected update modified stored Config")
	}
	obj.Spec.Content = updatedContent
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

type configBootstrapRaceStore struct {
	configBootstrapStore
	gets int
}

func (s *configBootstrapRaceStore) Get(ctx context.Context, name string, opts *metav1.GetOptions) (runtime.Object, error) {
	s.gets++
	if s.gets == 1 {
		return nil, apierrors.NewNotFound(ebsv1.Resource("configs"), name)
	}
	return s.configBootstrapStore.Get(ctx, name, opts)
}

func TestEnsureDefaultConfigConcurrentCreate(t *testing.T) {
	winner := &ebsv1.Config{
		ObjectMeta: metav1.ObjectMeta{Name: ebsv1.ServiceConfigName},
		Spec: ebsv1.ConfigSpec{
			Visibility: ebsv1.ConfigVisibilityOpsOnly,
			Content:    "artifactManagers: {default: {internalURL: 'http://art:8081'}}\ngitServer: {internalURL: 'http://git:8080'}",
		},
	}
	store := &configBootstrapRaceStore{
		configBootstrapStore: configBootstrapStore{objects: map[string]*ebsv1.Config{winner.Name: winner}},
	}
	candidate := winner.DeepCopy()
	candidate.Spec.Content = string(defaultServiceConfigTemplate)
	if err := ensureDefaultConfig(context.Background(), store, candidate); err != nil {
		t.Fatal(err)
	}
	if store.gets != 2 || store.creates != 0 || store.objects[winner.Name].Spec.Content != winner.Spec.Content {
		t.Fatal("concurrent initialization did not preserve and read the winning object")
	}
}

func TestEnsureDefaultConfigs(t *testing.T) {
	store := &configBootstrapStore{objects: make(map[string]*ebsv1.Config)}
	if err := ensureDefaultConfigs(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if store.creates != 3 {
		t.Fatalf("created %d configs, want 3", store.creates)
	}
	target := store.objects[ebsv1.BuildTargetConfigName]
	if errs := validation.ValidateConfig(target); len(errs) != 0 {
		t.Fatalf("default build-target Config: %v", errs)
	}
	resource := store.objects[ebsv1.BuildResourceConfigName]
	if errs := validation.ValidateConfig(resource); len(errs) != 0 {
		t.Fatalf("default build-resource Config: %v", errs)
	}
	serviceConfig := store.objects[ebsv1.ServiceConfigName]
	if errs := validation.ValidateConfig(serviceConfig); len(errs) != 0 {
		t.Fatalf("default services Config: %v", errs)
	}
	if target.Spec.Visibility != ebsv1.ConfigVisibilityPublic || resource.Spec.Visibility != ebsv1.ConfigVisibilityOpsOnly ||
		serviceConfig.Spec.Visibility != ebsv1.ConfigVisibilityOpsOnly {
		t.Fatal("incorrect default visibility")
	}
	var targets ebsv1.BuildTargetContent
	if err := yaml.UnmarshalStrict([]byte(target.Spec.Content), &targets); err != nil || len(targets.Targets) == 0 {
		t.Fatalf("target content: %v", err)
	}
	var resources ebsv1.BuildResourceContent
	if err := yaml.UnmarshalStrict([]byte(resource.Spec.Content), &resources); err != nil {
		t.Fatal(err)
	}
	if resources.Default.Requests["cpu"] != "4" || resources.Default.Requests["memory"] != "8Gi" {
		t.Fatalf("default resources: %+v", resources.Default)
	}
	var services ebsv1.ServiceConfigContent
	if err := yaml.UnmarshalStrict([]byte(serviceConfig.Spec.Content), &services); err != nil {
		t.Fatal(err)
	}
	if got := services.GitServer.InternalURL; got != "http://git-server:8080" {
		t.Fatalf("default Git Server URL: %q", got)
	}
	if got := services.GitServer.RunnerInternalURL; got != "http://127.0.0.1:8084" {
		t.Fatalf("default Git Server Runner URL: %q", got)
	}
	if got := services.ArtifactManagers["default"].InternalURL; got != "http://artifact-manager:8081" {
		t.Fatalf("default Compose artifact manager URL: %q", got)
	}
	if got := services.ArtifactManagers["default"].RunnerInternalURL; got != "http://127.0.0.1:8081" {
		t.Fatalf("default Compose Runner artifact manager URL: %q", got)
	}
	if got := services.ArtifactManagers["art-01"].InternalURL; got != "http://artifact-manager.art-01:8081" {
		t.Fatalf("default Kubernetes artifact manager URL: %q", got)
	}
	if services.ArtifactManagers["art-01"].RunnerInternalURL != "" || services.ArtifactManagers["art-01"].RunnerExternalURL != "" {
		t.Fatal("Kubernetes Runner URLs must be configured for the deployment")
	}
	target.Spec.Content = "operator change"
	serviceConfig.Spec.Content = "operator service change"
	if err := ensureDefaultConfigs(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if store.creates != 3 || store.objects[ebsv1.BuildTargetConfigName].Spec.Content != "operator change" ||
		store.objects[ebsv1.ServiceConfigName].Spec.Content != "operator service change" {
		t.Fatal("existing Config was overwritten")
	}
}

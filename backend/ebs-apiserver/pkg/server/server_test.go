package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emicklei/go-restful/v3"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/endpoints"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kube-openapi/pkg/validation/spec"

	ebsv1 "ebs-api/ebs/v1"
	iamv1 "ebs-api/iam/v1"
	ebsapi "ebs-apiserver/pkg/apis/ebs"
	iamapi "ebs-apiserver/pkg/apis/iam"
	buildstore "ebs-apiserver/pkg/registry/ebs/build"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	snapshotstore "ebs-apiserver/pkg/registry/ebs/snapshot"
	esclient "ebs-apiserver/pkg/storage/es"
)

func TestEBSSchemeRegistration(t *testing.T) {
	for _, tc := range []struct {
		version  string
		register func(*runtime.Scheme) error
	}{
		{"v1", ebsv1.AddToScheme},
		{runtime.APIVersionInternal, ebsapi.AddToScheme},
	} {
		scheme := runtime.NewScheme()
		if err := tc.register(scheme); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"Config", "Script", "Project", "Snapshot", "Build", "BuildInfo", "RpmRepo", "Job", "Runner"} {
			for _, suffix := range []string{"", "List"} {
				gvk := schema.GroupVersionKind{Group: ebsapi.GroupName, Version: tc.version, Kind: kind + suffix}
				for _, registered := range []*runtime.Scheme{scheme, Scheme} {
					if _, err := registered.New(gvk); err != nil {
						t.Fatalf("create %s: %v", gvk, err)
					}
				}
			}
		}
	}
}

func TestIAMSchemeRegistration(t *testing.T) {
	for _, tc := range []struct {
		version  string
		register func(*runtime.Scheme) error
	}{
		{"v1", iamv1.AddToScheme},
		{runtime.APIVersionInternal, iamapi.AddToScheme},
	} {
		scheme := runtime.NewScheme()
		if err := tc.register(scheme); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"User", "UserList", "MachineAccount", "MachineAccountList"} {
			gvk := schema.GroupVersionKind{Group: iamapi.GroupName, Version: tc.version, Kind: kind}
			for _, registered := range []*runtime.Scheme{scheme, Scheme} {
				if _, err := registered.New(gvk); err != nil {
					t.Fatalf("create %s: %v", gvk, err)
				}
			}
		}
	}
}

func TestOldJobsHTTPRoutes(t *testing.T) {
	client := esclient.NewClientForTesting("http://es", &http.Client{Transport: confTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ebs-oldjobs/_doc/project/job" {
			t.Fatalf("ES path=%s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"_id":"project/job","_seq_no":99,"_primary_term":1,"_source":{"data":{"apiVersion":"ebs/v1","kind":"Job","metadata":{"name":"job","namespace":"project","uid":"uid","resourceVersion":"123"},"spec":{"priority":1},"status":{"phase":"Succeeded"}}}}`))}, nil
	})})
	info, err := CreateAPIGroupInfo(testRESTOptions(), client)
	if err != nil {
		t.Fatal(err)
	}
	storage := info.VersionedResourcesStorageMap["v1"]
	container := restful.NewContainer()
	group := &endpoints.APIGroupVersion{
		Storage: map[string]rest.Storage{"oldjobs": storage["oldjobs"], "jobs": storage["jobs"], "jobs/archive": storage["jobs/archive"]},
		Root:    "/apis", GroupVersion: ebsv1.SchemeGroupVersion, OptionsExternalVersion: &schema.GroupVersion{Version: "v1"},
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
	for _, path := range []string{"/apis/ebs/v1/oldjobs", "/apis/ebs/v1/namespaces/{namespace}/oldjobs", "/apis/ebs/v1/namespaces/{namespace}/oldjobs/{name}"} {
		if strings.Join(methods[path], ",") != "GET" {
			t.Fatalf("route %s methods=%v", path, methods[path])
		}
	}
	if strings.Join(methods["/apis/ebs/v1/namespaces/{namespace}/jobs/{name}/archive"], ",") != "POST" {
		t.Fatalf("archive route=%v", methods)
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
		if tc.status == 200 && (!strings.Contains(w.Body.String(), `"resourceVersion":"123"`) || strings.Contains(w.Body.String(), `"spec"`)) {
			t.Fatalf("projected Job=%s", w.Body)
		}
	}
}

func TestCompleteLoadsElasticsearchAPIKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(keyFile, []byte("encoded-key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := NewEulerMakerServerOptions()
	opts.EsAPIKeyFile = keyFile
	if err := opts.Complete(); err != nil {
		t.Fatalf("complete options: %v", err)
	}
	if opts.esConfig.APIKey != "encoded-key" {
		t.Fatal("Elasticsearch API key was not loaded")
	}
}

func TestOpenAPIDefinitionsExposeObjectFields(t *testing.T) {
	definitions := getOpenAPIDefinitions(func(path string) spec.Ref {
		return spec.MustCreateRef("#/components/schemas/" + path)
	})
	tests := map[string][]string{
		"ebs-api/ebs/v1.RpmRepoRepositoryStatus": {"repositoryUID", "contentURL"},
		"ebs-api/ebs/v1.RpmRepoReleaseStatus":    {"phase", "contentURL"},
		"ebs-api/ebs/v1.PackageRepo":             {"name", "url", "ref"},
		"ebs-api/ebs/v1.BuildSpec":               {"buildTarget", "packages"},
		"ebs-api/ebs/v1.BuildInfoSpec":           {"bootstrapRepo", "buildPayload"},
		"ebs-api/ebs/v1.BuildInfoStatus":         {"failedPackages"},
		"ebs-api/ebs/v1.BaseBuildRef":            {"name"},
		"ebs-api/ebs/v1.BuildStatus":             {"phase", "stage", "startTime", "endTime", "baseBuildRef", "conditions"},
		"ebs-api/ebs/v1.ProjectSpec":             {"displayName", "buildTargets", "packageRepos", "bootstrapRepo"},
		"ebs-api/ebs/v1.BuildResourceContent":    {"default", "packages"},
		"ebs-api/ebs/v1.PackageResourceConfig":   {"default", "arches"},
		"ebs-api/ebs/v1.JobSpec":                 {"priority", "runtime", "runtimeSpec", "payload"},
		"ebs-api/ebs/v1.SnapshotStatus":          {"phase", "conditions"},
		"ebs-api/ebs/v1.RunnerStatus":            {"phase", "capacity", "heartbeat"},
		"ebs-api/iam/v1.UserSpec":                {"enabled", "scopes", "email"},
		"ebs-api/iam/v1.MachineAccount":          {"apiVersion", "kind", "metadata", "spec"},
	}
	for name, fields := range tests {
		definition, ok := definitions[name]
		if !ok {
			t.Errorf("OpenAPI definition %q is missing", name)
			continue
		}
		for _, field := range fields {
			if _, ok := definition.Schema.Properties[field]; !ok {
				t.Errorf("OpenAPI definition %q is missing property %q", name, field)
			}
		}
	}

	snapshotSpec, ok := definitions["ebs-api/ebs/v1.SnapshotSpec"]
	if !ok {
		t.Fatal("SnapshotSpec OpenAPI definition is missing")
	}
	if _, ok := snapshotSpec.Schema.Properties["packageRepos"]; !ok {
		t.Error("SnapshotSpec OpenAPI definition must expose packageRepos")
	}
	snapshotStatus, ok := definitions["ebs-api/ebs/v1.SnapshotStatus"]
	if !ok {
		t.Fatal("SnapshotStatus OpenAPI definition is missing")
	}
	if _, ok := snapshotStatus.Schema.Properties["packageRepoStatuses"]; !ok {
		t.Error("SnapshotStatus OpenAPI definition must expose packageRepoStatuses")
	}

}

func TestCodecsUseStrictDecoding(t *testing.T) {
	tests := []string{
		`{"apiVersion":"ebs/v1","kind":"Runner","metadata":{"name":"runner-a"},"spec":{"unknown":true}}`,
		`{"apiVersion":"ebs/v1","kind":"Runner","metadata":{"name":"runner-a"},"spec":{"arch":1}}`,
		`{"apiVersion":"other/v1","kind":"Runner","metadata":{"name":"runner-a"}}`,
	}
	for _, data := range tests {
		if _, _, err := Codecs.UniversalDeserializer().Decode([]byte(data), nil, nil); err == nil {
			t.Errorf("expected strict decoding to reject %s", data)
		}
	}
}

func TestBuildInfoFailedPackagesDecoding(t *testing.T) {
	data := []byte(`{"apiVersion":"ebs/v1","kind":"BuildInfo","metadata":{"name":"build-a","namespace":"project-a"},"status":{"failedPackages":["gcc","glibc"]}}`)
	obj, _, err := Codecs.UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatalf("decode BuildInfo with failedPackages: %v", err)
	}
	buildInfo, ok := obj.(*ebsv1.BuildInfo)
	if !ok {
		t.Fatalf("decoded object has type %T, want *BuildInfo", obj)
	}
	if len(buildInfo.Status.FailedPackages) != 2 || buildInfo.Status.FailedPackages[0] != "gcc" || buildInfo.Status.FailedPackages[1] != "glibc" {
		t.Fatalf("failedPackages = %v, want [gcc glibc]", buildInfo.Status.FailedPackages)
	}
}

func TestCompleteStoreInitializesStatusStorage(t *testing.T) {
	storeOptions := &generic.StoreOptions{RESTOptions: testRESTOptions()}

	tests := []struct {
		name  string
		store *genericregistry.Store
	}{
		{name: "projects/status", store: projectstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
		{name: "snapshots/status", store: snapshotstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
		{name: "builds/status", store: buildstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.store.Storage.Storage != nil {
				t.Fatalf("expected storage to start unset")
			}
			if err := completeStore(tt.store, storeOptions); err != nil {
				t.Fatalf("complete store: %v", err)
			}
			if tt.store.Storage.Storage == nil {
				t.Fatalf("expected storage to be initialized")
			}
		})
	}
}

func TestEnableIAMFlag(t *testing.T) {
	opts := NewEulerMakerServerOptions()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	opts.AddFlags(fs)
	if err := fs.Parse([]string{"--enable-iam"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if !opts.EnableIAM {
		t.Fatal("--enable-iam did not enable IAM")
	}
}

func TestConfigRequestBodyLimit(t *testing.T) {
	opts := NewEulerMakerServerOptions()
	opts.InsecureAllowUnauthenticatedClients = true
	opts.RecommendedOptions.SecureServing.BindPort = 0
	config, err := opts.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := config.MaxRequestBodyBytes; got != 5<<20 {
		t.Fatalf("request body limit = %d, want 5 MiB", got)
	}
}

func TestInternalClientCARequired(t *testing.T) {
	opts := NewEulerMakerServerOptions()
	if err := opts.validateInternalClientAuthentication(); err == nil {
		t.Fatal("missing internal client CA was accepted")
	}
	opts.InsecureAllowUnauthenticatedClients = true
	if err := opts.validateInternalClientAuthentication(); err != nil {
		t.Fatal(err)
	}
}

func TestIAMStorageUsesESWithoutWatch(t *testing.T) {
	client := esclient.NewClientForTesting("http://unused", http.DefaultClient)
	group, _, _ := CreateIAMAPIGroupInfo(client)
	users := group.VersionedResourcesStorageMap["v1"]["users"]
	if users == nil {
		t.Fatal("users storage was not installed")
	}
	if _, ok := users.(rest.Watcher); ok {
		t.Fatal("users unexpectedly implements watch")
	}
	machines := group.VersionedResourcesStorageMap["v1"]["machineaccounts"]
	if machines == nil {
		t.Fatal("machineaccounts storage was not installed")
	}
	if _, ok := machines.(rest.Creater); ok {
		t.Fatal("machineaccounts unexpectedly implements create")
	}
	if _, ok := machines.(rest.Updater); ok {
		t.Fatal("machineaccounts unexpectedly implements update")
	}
}

func testRESTOptions() generic.RESTOptions {
	return generic.RESTOptions{
		StorageConfig: &storagebackend.ConfigForResource{
			Config: storagebackend.Config{
				Codec: Codecs.LegacyCodec(ebsv1.SchemeGroupVersion),
				EncodeVersioner: runtime.NewMultiGroupVersioner(
					ebsv1.SchemeGroupVersion,
					schema.GroupKind{Group: ebsapi.GroupName},
				),
			},
		},
		Decorator:      fakeStorageDecorator,
		ResourcePrefix: "test",
	}
}

func TestStorageCapabilitiesFollowPrimaryStore(t *testing.T) {
	apiGroup, err := CreateAPIGroupInfo(
		testRESTOptions(),
		esclient.NewClientForTesting("http://unused", http.DefaultClient),
	)
	if err != nil {
		t.Fatalf("create API group info: %v", err)
	}
	storageMap := apiGroup.VersionedResourcesStorageMap["v1"]
	if _, ok := storageMap["buildresourceconfigs"]; ok {
		t.Fatal("BuildResourceConfig must not register a global API storage")
	}
	for _, resource := range []string{"configs", "scripts"} {
		store := storageMap[resource]
		if store == nil {
			t.Errorf("%s must register API storage", resource)
			continue
		}
		if _, ok := store.(rest.Lister); !ok {
			t.Errorf("%s must implement list", resource)
		}
		if _, ok := store.(rest.CreaterUpdater); !ok {
			t.Errorf("%s must implement create and update", resource)
		}
		if _, ok := store.(rest.Watcher); ok {
			t.Errorf("%s must not implement watch", resource)
		}
		if _, ok := store.(rest.CollectionDeleter); !ok {
			t.Errorf("%s must implement collection delete", resource)
		}
		if _, ok := store.(rest.GracefulDeleter); !ok {
			t.Errorf("%s must implement delete", resource)
		}
	}

	for _, resource := range []string{"projects", "snapshots", "builds", "buildinfos", "rpmrepos"} {
		if _, ok := storageMap[resource].(rest.Watcher); ok {
			t.Errorf("%s unexpectedly implements watch", resource)
		}
	}
	for _, resource := range []string{"jobs", "runners"} {
		if _, ok := storageMap[resource].(rest.Watcher); !ok {
			t.Errorf("%s must implement watch", resource)
		}
	}
	for _, resource := range []string{"jobs/status", "runners/status"} {
		status := storageMap[resource]
		if _, ok := status.(rest.Getter); !ok {
			t.Errorf("%s must implement get", resource)
		}
		if _, ok := status.(rest.Updater); !ok {
			t.Errorf("%s must implement update", resource)
		}
		if _, ok := status.(rest.Creater); ok {
			t.Errorf("%s unexpectedly implements create", resource)
		}
		if _, ok := status.(rest.Lister); ok {
			t.Errorf("%s unexpectedly implements list", resource)
		}
		if _, ok := status.(rest.Watcher); ok {
			t.Errorf("%s unexpectedly implements watch", resource)
		}
		if _, ok := status.(rest.GracefulDeleter); ok {
			t.Errorf("%s unexpectedly implements delete", resource)
		}
	}
	if _, ok := storageMap["builds/abort"].(rest.Connecter); !ok {
		t.Error("builds/abort must implement rest.Connecter")
	}
	if _, ok := storageMap["jobs/abort"].(rest.Connecter); !ok {
		t.Error("jobs/abort must implement rest.Connecter")
	}
}

func TestJobStorageUsesResourcePrefixForKeys(t *testing.T) {
	apiGroup, err := CreateAPIGroupInfo(
		testRESTOptions(),
		esclient.NewClientForTesting("http://unused", http.DefaultClient),
	)
	if err != nil {
		t.Fatalf("create API group info: %v", err)
	}

	store := apiGroup.VersionedResourcesStorageMap["v1"]["jobs"].(*genericregistry.Store)
	ctx := genericapirequest.WithNamespace(genericapirequest.NewContext(), "example-project")
	if got, want := store.KeyRootFunc(ctx), "/test/example-project"; got != want {
		t.Fatalf("job key root = %q, want %q", got, want)
	}
	if got, err := store.KeyFunc(ctx, "example-job"); err != nil {
		t.Fatalf("job key: %v", err)
	} else if want := "/test/example-project/example-job"; got != want {
		t.Fatalf("job key = %q, want %q", got, want)
	}
}

func fakeStorageDecorator(
	config *storagebackend.ConfigForResource,
	resourcePrefix string,
	keyFunc func(obj runtime.Object) (string, error),
	newFunc func() runtime.Object,
	newListFunc func() runtime.Object,
	getAttrsFunc storage.AttrFunc,
	trigger storage.IndexerFuncs,
	indexers *cache.Indexers,
) (storage.Interface, factory.DestroyFunc, error) {
	return &fakeStorage{}, func() {}, nil
}

type fakeStorage struct{}

func (fakeStorage) Versioner() storage.Versioner {
	return storage.APIObjectVersioner{}
}

func (fakeStorage) Create(ctx context.Context, key string, obj, out runtime.Object, ttl uint64) error {
	return errors.New("not implemented")
}

func (fakeStorage) Delete(ctx context.Context, key string, out runtime.Object, preconditions *storage.Preconditions, validateDeletion storage.ValidateObjectFunc, cachedExistingObject runtime.Object) error {
	return errors.New("not implemented")
}

func (fakeStorage) Watch(ctx context.Context, key string, opts storage.ListOptions) (watch.Interface, error) {
	return nil, errors.New("not implemented")
}

func (fakeStorage) Get(ctx context.Context, key string, opts storage.GetOptions, objPtr runtime.Object) error {
	return errors.New("not implemented")
}

func (fakeStorage) GetList(ctx context.Context, key string, opts storage.ListOptions, listObj runtime.Object) error {
	return errors.New("not implemented")
}

func (fakeStorage) GuaranteedUpdate(ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool, preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, cachedExistingObject runtime.Object) error {
	return errors.New("not implemented")
}

func (fakeStorage) Count(key string) (int64, error) {
	return 0, errors.New("not implemented")
}

func (fakeStorage) RequestWatchProgress(ctx context.Context) error {
	return errors.New("not implemented")
}

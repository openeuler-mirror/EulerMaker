package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kube-openapi/pkg/validation/spec"

	ebsapi "ebs-apiserver/pkg/apis/ebs"
	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	buildstore "ebs-apiserver/pkg/registry/ebs/build"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	snapshotstore "ebs-apiserver/pkg/registry/ebs/snapshot"
	esclient "ebs-apiserver/pkg/storage/es"
)

func TestOpenAPIDefinitionsExposeObjectFields(t *testing.T) {
	definitions := getOpenAPIDefinitions(func(path string) spec.Ref {
		return spec.MustCreateRef("#/components/schemas/" + path)
	})

	tests := map[string][]string{
		"ebs-apiserver/pkg/apis/ebs/v1.ProjectSpec":    {"displayName", "buildTargets", "packageRepos"},
		"ebs-apiserver/pkg/apis/ebs/v1.JobSpec":        {"priority", "runtime", "runtimeSpec", "payload"},
		"ebs-apiserver/pkg/apis/ebs/v1.RunnerStatus":   {"phase", "capacity", "heartbeat"},
		"ebs-apiserver/pkg/apis/iam/v1.UserSpec":       {"enabled", "scopes", "email"},
		"ebs-apiserver/pkg/apis/iam/v1.MachineAccount": {"apiVersion", "kind", "metadata", "spec"},
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

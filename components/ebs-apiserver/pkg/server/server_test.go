package server

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	"k8s.io/client-go/tools/cache"

	ebsapi "ebs-apiserver/pkg/apis/ebs"
	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
	buildstore "ebs-apiserver/pkg/registry/ebs/build"
	jobstore "ebs-apiserver/pkg/registry/ebs/job"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	runnerstore "ebs-apiserver/pkg/registry/ebs/runner"
	snapshotstore "ebs-apiserver/pkg/registry/ebs/snapshot"
)

func TestCompleteStoreInitializesStatusStorage(t *testing.T) {
	storeOptions := &generic.StoreOptions{RESTOptions: generic.RESTOptions{
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
	}}

	tests := []struct {
		name  string
		store *genericregistry.Store
	}{
		{name: "projects/status", store: projectstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
		{name: "snapshots/status", store: snapshotstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
		{name: "builds/status", store: buildstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
		{name: "jobs/status", store: jobstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
		{name: "runners/status", store: runnerstore.NewStorage(Scheme).Status.(*genericregistry.Store)},
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
	return fakeStorage{}, func() {}, nil
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

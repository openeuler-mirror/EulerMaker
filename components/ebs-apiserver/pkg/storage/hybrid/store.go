package hybrid

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	"ebs-apiserver/pkg/storage/es"
)

type EnricherStore struct {
	etcdStore   *genericregistry.Store
	esClient    *es.Client
	resource    string
	newFunc     func() runtime.Object
	newListFunc func() runtime.Object
}

func NewEnricherStore(
	etcdStore *genericregistry.Store,
	esClient *es.Client,
	resource string,
	newFunc func() runtime.Object,
	newListFunc func() runtime.Object,
) *EnricherStore {
	return &EnricherStore{
		etcdStore:   etcdStore,
		esClient:    esClient,
		resource:    resource,
		newFunc:     newFunc,
		newListFunc: newListFunc,
	}
}

func (e *EnricherStore) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	var (
		obj     runtime.Object
		esData  json.RawMessage
		etcdErr error
		esErr   error
		wg      sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		obj, etcdErr = e.etcdStore.Get(ctx, name, options)
	}()
	go func() {
		defer wg.Done()
		esDocID := name
		if ns, ok := genericapirequest.NamespaceFrom(ctx); ok && ns != "" {
			esDocID = ns + "/" + name
		}
		esData, esErr = e.esClient.Get(ctx, e.resource, esDocID)
	}()

	wg.Wait()

	if etcdErr != nil {
		return nil, etcdErr
	}

	if esErr == nil && esData != nil {
		if err := json.Unmarshal(esData, obj); err != nil {
			return obj, nil
		}
	}

	return obj, nil
}

func (e *EnricherStore) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	return e.etcdStore.List(ctx, options)
}

func (e *EnricherStore) Create(ctx context.Context, obj runtime.Object, createValidation rest.ValidateObjectFunc, options *metav1.CreateOptions) (runtime.Object, error) {
	if err := e.indexES(ctx, obj); err != nil {
		return nil, fmt.Errorf("ES index failed: %w", err)
	}

	return e.etcdStore.Create(ctx, obj, createValidation, options)
}

func (e *EnricherStore) Update(ctx context.Context, name string, objInfo rest.UpdatedObjectInfo, createValidation rest.ValidateObjectFunc, updateValidation rest.ValidateObjectUpdateFunc, forceAllowCreate bool, options *metav1.UpdateOptions) (runtime.Object, bool, error) {
	result, wasCreated, err := e.etcdStore.Update(ctx, name, objInfo, createValidation, updateValidation, forceAllowCreate, options)
	if err != nil {
		return nil, false, err
	}

	if err := e.indexES(ctx, result); err != nil {
		return result, wasCreated, nil
	}

	return result, wasCreated, nil
}

func (e *EnricherStore) Delete(ctx context.Context, name string, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	obj, wasImmediate, err := e.etcdStore.Delete(ctx, name, deleteValidation, options)
	if err != nil {
		return nil, false, err
	}

	esDocID := name
	if ns, ok := genericapirequest.NamespaceFrom(ctx); ok && ns != "" {
		esDocID = ns + "/" + name
	}
	e.esClient.Delete(ctx, e.resource, esDocID)

	return obj, wasImmediate, nil
}

func (e *EnricherStore) Watch(ctx context.Context, options *internalversion.ListOptions) (watch.Interface, error) {
	return e.etcdStore.Watch(ctx, options)
}

func (e *EnricherStore) New() runtime.Object {
	return e.newFunc()
}

func (e *EnricherStore) NewList() runtime.Object {
	return e.newListFunc()
}

func (e *EnricherStore) DeleteCollection(ctx context.Context, deleteValidation rest.ValidateObjectFunc, options *metav1.DeleteOptions, listOptions *internalversion.ListOptions) (runtime.Object, error) {
	return e.etcdStore.DeleteCollection(ctx, deleteValidation, options, listOptions)
}

func (e *EnricherStore) NamespaceScoped() bool {
	return e.etcdStore.NamespaceScoped()
}

func (e *EnricherStore) GetSingularName() string {
	return e.etcdStore.GetSingularName()
}

func (e *EnricherStore) Destroy() {
}

func (e *EnricherStore) ConvertToTable(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	return e.etcdStore.ConvertToTable(ctx, object, tableOptions)
}

func (e *EnricherStore) indexES(ctx context.Context, obj runtime.Object) error {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return fmt.Errorf("get object metadata: %w", err)
	}
	name := accessor.GetName()
	if name == "" {
		return nil
	}

	ns := accessor.GetNamespace()
	docID := name
	if ns != "" {
		docID = ns + "/" + name
	}

	data, err := json.Marshal(obj)
	if err != nil {
		return fmt.Errorf("marshal object for ES: %w", err)
	}

	return e.esClient.Index(ctx, e.resource, docID, data)
}

var _ rest.StandardStorage = (*EnricherStore)(nil)

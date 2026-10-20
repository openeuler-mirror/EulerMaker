package esstore

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	graphstore "ebs-apiserver/pkg/registry/ebs/buildgraph"
	"ebs-apiserver/pkg/storage/es"
)

type graphOwnerGetter struct{ info *ebsv1.BuildInfo }

func (g graphOwnerGetter) New() runtime.Object { return &ebsv1.BuildInfo{} }
func (g graphOwnerGetter) Destroy()            {}
func (g graphOwnerGetter) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	return g.info.DeepCopy(), nil
}

func TestBuildGraphStorage(t *testing.T) {
	fake := &fakeES{}
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(fake.roundTrip)})
	templates := graphstore.NewStorage()
	store := New(client, "buildgraph", "BuildGraph", templates.Resource.(*genericregistry.Store))
	status := NewStatus(store, templates.Status.(*genericregistry.Store))
	owner := &ebsv1.BuildInfo{ObjectMeta: metav1.ObjectMeta{Name: "build-a", Namespace: "project-a", UID: "owner-uid"}}
	store.SetCreateHook(graphstore.ValidateOwner(graphOwnerGetter{owner}))
	graph := &ebsv1.BuildGraph{
		ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(owner, ebsv1.SchemeGroupVersion.WithKind("BuildInfo"))}},
		Status: ebsv1.BuildGraphStatus{Nodes: map[string]ebsv1.DcgNodeState{"a": {}, "b": {}}},
	}
	ctx := request.WithNamespace(context.Background(), owner.Namespace)
	obj, err := store.Create(ctx, graph, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	created := obj.(*ebsv1.BuildGraph)
	if created.UID == "" || created.ResourceVersion == "" || len(created.Status.Nodes) != 2 {
		t.Fatal("create lost identity or graph")
	}
	updated := created.DeepCopy()
	updated.Annotations = map[string]string{"ignored": "status-write"}
	updated.Status.Nodes["b"] = ebsv1.DcgNodeState{InstallInDep: map[string]ebsv1.VersionConst{"a": {}}}
	updated.Status.Nodes["a"] = ebsv1.DcgNodeState{OutDep: []string{"b"}}
	obj, _, err = status.Update(ctx, graph.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved := obj.(*ebsv1.BuildGraph)
	if saved.ResourceVersion == created.ResourceVersion || !reflect.DeepEqual(saved.OwnerReferences, created.OwnerReferences) || len(saved.Annotations) != 0 {
		t.Fatal("incorrect status version/metadata")
	}
	if _, _, err = status.Update(ctx, graph.Name, rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, nil); !apierrors.IsConflict(err) {
		t.Fatalf("stale write: %v", err)
	}
	ordinary := saved.DeepCopy()
	ordinary.Status.Nodes = nil
	ordinary.Labels = map[string]string{"test": "value"}
	obj, _, err = store.Update(ctx, graph.Name, rest.DefaultUpdatedObjectInfo(ordinary), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(obj.(*ebsv1.BuildGraph).Status, saved.Status) {
		t.Fatal("ordinary update overwrote graph")
	}
	owner.UID = "recreated-owner"
	if _, err = store.Create(ctx, graph.DeepCopy(), nil, nil); !apierrors.IsInvalid(err) {
		t.Fatalf("owner UID mismatch: %v", err)
	}
}

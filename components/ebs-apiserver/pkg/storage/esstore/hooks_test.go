package esstore

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	"ebs-apiserver/pkg/storage/es"
)

func hookStore() (*Store, *StatusStore, *int) {
	fake := &fakeES{}
	writes := new(int)
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut || r.Method == http.MethodDelete {
			*writes++
		}
		if r.Method == http.MethodDelete {
			return jsonResponse(200, map[string]string{"result": "deleted"}), nil
		}
		return fake.roundTrip(r)
	})})
	template := projectstore.NewStorage(runtime.NewScheme())
	store := New(client, "project", "Project", template.Project.(*genericregistry.Store))
	return store, NewStatus(store, template.Status.(*genericregistry.Store)), writes
}
func hookProject() *ebsv1.Project {
	return &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}, Spec: ebsv1.ProjectSpec{BuildTargets: []ebsv1.BuildTarget{{Os: "os", Arch: "arch"}}}}
}

func TestCreateTransactionOrdering(t *testing.T) {
	for _, dry := range []bool{false, true} {
		store, _, writes := hookStore()
		var order []string
		store.SetCreateHook(func(context.Context, runtime.Object) error { order = append(order, "validate"); return nil })
		store.SetCreateTransaction(func(ctx context.Context, obj runtime.Object, dryRun bool, persist func() (runtime.Object, error)) (runtime.Object, error) {
			order = append(order, "transaction")
			if dryRun != dry {
				t.Fatal("dry-run flag mismatch")
			}
			p := obj.(*ebsv1.Project)
			if p.UID == "" || p.Status.Phase != ebsv1.ProjectActive {
				t.Fatal("identity/defaults not prepared")
			}
			result, err := persist()
			if err == nil && result.(*ebsv1.Project).UID != p.UID {
				t.Fatal("UID changed during persistence")
			}
			return result, err
		})
		options := &metav1.CreateOptions{}
		if dry {
			options.DryRun = []string{metav1.DryRunAll}
		}
		_, err := store.Create(request.WithNamespace(context.Background(), ""), hookProject(), func(context.Context, runtime.Object) error { order = append(order, "admission"); return nil }, options)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(order, []string{"validate", "admission", "transaction"}) {
			t.Fatal(order)
		}
		if (*writes == 0) != dry {
			t.Fatalf("writes=%d dry=%v", *writes, dry)
		}
	}
	store, _, _ := hookStore()
	store.SetCreateTransaction(func(context.Context, runtime.Object, bool, func() (runtime.Object, error)) (runtime.Object, error) {
		t.Fatal("transaction ran after failed admission")
		return nil, nil
	})
	want := errors.New("denied")
	if _, err := store.Create(request.WithNamespace(context.Background(), ""), hookProject(), func(context.Context, runtime.Object) error { return want }, nil); err != want {
		t.Fatal(err)
	}
}

func TestPostWriteHooks(t *testing.T) {
	ctx := request.WithNamespace(context.Background(), "")
	store, status, _ := hookStore()
	obj, err := store.Create(ctx, hookProject(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var deleted []bool
	store.SetAfterWrite(func(_ context.Context, obj runtime.Object, d bool) { deleted = append(deleted, d) })
	next := obj.(*ebsv1.Project).DeepCopy()
	next.Status.Phase = ebsv1.ProjectTerminating
	if _, _, err := status.Update(ctx, next.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, &metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatal("dry-run notified")
	}
	obj, _, err = status.Update(ctx, next.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	badUID := types.UID("wrong")
	if _, _, err := store.Delete(ctx, next.Name, nil, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &badUID}}); err == nil {
		t.Fatal("bad UID accepted")
	}
	if !reflect.DeepEqual(deleted, []bool{false}) {
		t.Fatal(deleted)
	}
	if _, _, err := store.Delete(ctx, next.Name, nil, &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []bool{false}) {
		t.Fatal("dry-run delete notified")
	}
	if _, _, err := store.Delete(ctx, next.Name, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(deleted, []bool{false, true}) {
		t.Fatal(deleted)
	}
}

func TestFinalizerCompletionNotifiesDeletion(t *testing.T) {
	store, _, _ := hookStore()
	ctx := request.WithNamespace(context.Background(), "")
	p := hookProject()
	p.Finalizers = []string{"example.com/hold"}
	if _, err := store.Create(ctx, p, nil, nil); err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.SetAfterWrite(func(_ context.Context, _ runtime.Object, deleted bool) {
		if !deleted {
			t.Fatal("expected physical deletion")
		}
		calls++
	})
	obj, deleted, err := store.Delete(ctx, p.Name, nil, nil)
	if err != nil || deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	if calls != 0 {
		t.Fatal("released before finalizer completed")
	}
	next := obj.(*ebsv1.Project).DeepCopy()
	next.Finalizers = nil
	if _, _, err := store.Update(ctx, p.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("deletion notifications=%d", calls)
	}
}

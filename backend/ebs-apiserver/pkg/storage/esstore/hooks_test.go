package esstore

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	projectstore "ebs-apiserver/pkg/registry/ebs/project"
	rpmrepostore "ebs-apiserver/pkg/registry/ebs/rpmrepo"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
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
	template := projectstore.NewStorage()
	store := New(client, "project", "Project", template.Resource.(*genericregistry.Store))
	return store, NewStatus(store, template.Status.(*genericregistry.Store)), writes
}
func hookProject() *ebsv1.Project {
	return &ebsv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "project-a"}, Spec: ebsv1.ProjectSpec{Distro: "os", BuildArches: []string{"arch"}}}
}

func TestUpdateHookOnlyRunsForOrdinaryUpdates(t *testing.T) {
	store, status, _ := hookStore()
	ctx := request.WithNamespace(context.Background(), "")
	created, err := store.Create(ctx, hookProject(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.SetUpdateHook(func(context.Context, runtime.Object, runtime.Object) error {
		calls++
		return nil
	})
	project := created.(*ebsv1.Project).DeepCopy()
	project.Status.Phase = ebsv1.ProjectTerminating
	updated, _, err := status.Update(ctx, project.Name, rest.DefaultUpdatedObjectInfo(project), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("status update called ordinary update hook %d times", calls)
	}
	project = updated.(*ebsv1.Project).DeepCopy()
	project.Spec.Description = "updated"
	if _, _, err := store.Update(ctx, project.Name, rest.DefaultUpdatedObjectInfo(project), nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("ordinary update hook calls = %d, want 1", calls)
	}
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

func TestRpmRepoDeletionRequiresDrainedFinalizer(t *testing.T) {
	fake := &fakeES{}
	deletes := 0
	client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			deletes++
			return jsonResponse(200, map[string]string{"result": "deleted"}), nil
		}
		return fake.roundTrip(r)
	})})
	template := rpmrepostore.NewStorage()
	store := New(client, "rpmrepo", "RpmRepo", template.Resource.(*genericregistry.Store))
	status := NewStatus(store, template.Status.(*genericregistry.Store))
	ctx := request.WithNamespace(context.Background(), "project-a")
	repo := &ebsv1.RpmRepo{ObjectMeta: metav1.ObjectMeta{
		Name: "build-a", Namespace: "project-a", Labels: map[string]string{ebsv1.BuildTargetArchLabel: "aarch64"},
	}}
	if _, err := store.Create(ctx, repo, nil, nil); err != nil {
		t.Fatal(err)
	}
	obj, deleted, err := store.Delete(ctx, repo.Name, nil, nil)
	if err != nil || deleted || deletes != 0 {
		t.Fatalf("initial delete=%t error=%v", deleted, err)
	}
	next := obj.(*ebsv1.RpmRepo).DeepCopy()
	if next.DeletionTimestamp == nil || !slices.Contains(next.Finalizers, ebsv1.RpmRepoCleanupFinalizer) {
		t.Fatal("deletion did not retain cleanup finalizer")
	}
	next.Finalizers = nil
	if _, _, err := store.Update(ctx, repo.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil); err == nil {
		t.Fatal("unacknowledged cleanup allowed finalizer removal")
	}
	next = obj.(*ebsv1.RpmRepo).DeepCopy()
	next.Status.Conditions = []metav1.Condition{{Type: ebsv1.RpmRepoConditionCleanupReady, Status: metav1.ConditionTrue,
		Reason: ebsv1.RpmRepoReasonOperationsDrained, LastTransitionTime: metav1.Now()}}
	obj, _, err = status.Update(ctx, repo.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	next = obj.(*ebsv1.RpmRepo).DeepCopy()
	next.Finalizers = nil
	if _, _, err := store.Update(ctx, repo.Name, rest.DefaultUpdatedObjectInfo(next), nil, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatalf("physical deletes=%d", deletes)
	}
}

func TestBuildDeletionValidationBeforePersistence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   ebsv1.BuildPhase
		stage   ebsv1.BuildStage
		allowed bool
	}{
		{"processing", ebsv1.BuildProcessing, ebsv1.BuildStageBuild, false},
		{"published", ebsv1.BuildSuccess, ebsv1.BuildStagePublish, false},
		{"single succeeded", ebsv1.BuildSuccess, ebsv1.BuildStageBuild, true},
		{"aborted", ebsv1.BuildAborted, ebsv1.BuildStageBuild, true},
	} {
		for _, mode := range []string{"delete", "dry-run", "collection", "collection dry-run"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				build := &ebsv1.Build{
					ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "project", UID: "uid",
						Finalizers: []string{ebsv1.BuildCleanupFinalizer}},
					Status: ebsv1.BuildStatus{Phase: tc.phase, Stage: tc.stage},
				}
				document, err := json.Marshal(map[string]interface{}{"data": build})
				if err != nil {
					t.Fatal(err)
				}
				fake := &fakeES{document: document, seqNo: 7}
				writes := 0
				client := es.NewClientForTesting("http://es", &http.Client{Transport: fakeRoundTripper(func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/ebs-builds/_pit":
						return jsonResponse(200, map[string]string{"id": "pit"}), nil
					case "/_pit":
						return jsonResponse(200, map[string]bool{"succeeded": true}), nil
					case "/_search":
						return jsonResponse(200, map[string]interface{}{
							"pit_id": "pit", "hits": map[string]interface{}{
								"total": map[string]int{"value": 1},
								"hits": []interface{}{map[string]interface{}{
									"_id": "project/build", "_seq_no": fake.seqNo, "_primary_term": 1,
									"_source": json.RawMessage(fake.document),
								}},
							},
						}), nil
					}
					if r.Method == http.MethodPut || r.Method == http.MethodDelete {
						writes++
						if r.Method != http.MethodPut || r.URL.Query().Get("if_seq_no") != "7" ||
							r.URL.Query().Get("if_primary_term") != "1" {
							t.Fatalf("unexpected deletion write: %s %s", r.Method, r.URL)
						}
					}
					return fake.roundTrip(r)
				})})
				template := scopedresource.NewStorage(scopedresource.Config{
					Resource: "builds", Singular: "build", Kind: "Build",
					New:     func() runtime.Object { return &ebsv1.Build{} },
					NewList: func() runtime.Object { return &ebsv1.BuildList{} },
					ValidateDelete: func(obj runtime.Object) field.ErrorList {
						return validation.ValidateBuildDelete(obj.(*ebsv1.Build))
					},
				})
				store := New(client, "build", "Build", template.Resource.(*genericregistry.Store))
				ctx := request.WithNamespace(context.Background(), "project")
				opts := &metav1.DeleteOptions{}
				dryRun := mode == "dry-run" || mode == "collection dry-run"
				if dryRun {
					opts.DryRun = []string{metav1.DryRunAll}
				}
				if mode == "collection" || mode == "collection dry-run" {
					_, err = store.DeleteCollection(ctx, nil, opts, &internalversion.ListOptions{})
				} else {
					_, _, err = store.Delete(ctx, build.Name, nil, opts)
				}
				if tc.allowed && err != nil || !tc.allowed && !apierrors.IsConflict(err) {
					t.Fatalf("allowed=%t, error=%v", tc.allowed, err)
				}
				wantWrites := 0
				if tc.allowed && !dryRun {
					wantWrites = 1
				}
				if writes != wantWrites {
					t.Fatalf("writes=%d, want %d", writes, wantWrites)
				}
				current, err := store.Get(ctx, build.Name, nil)
				if err != nil {
					t.Fatal(err)
				}
				persisted := current.(*ebsv1.Build)
				if (persisted.DeletionTimestamp != nil) != (wantWrites == 1) ||
					!slices.Equal(persisted.Finalizers, build.Finalizers) || !reflect.DeepEqual(persisted.Status, build.Status) {
					t.Fatalf("unexpected persisted build: %+v", persisted)
				}
				if wantWrites == 1 {
					if _, _, err := store.Delete(ctx, build.Name, nil, nil); err != nil || writes != 1 {
						t.Fatalf("repeated delete: writes=%d, error=%v", writes, err)
					}
				}
			})
		}
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

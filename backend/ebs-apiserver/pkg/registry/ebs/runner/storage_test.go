package runner

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/registry/generic"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

type deletionStorage struct {
	storage.Interface
	initial *ebsv1.Runner
	current *ebsv1.Runner
	deleted bool
	lastKey string
}

func newDeletionStorage(t *testing.T, backend *deletionStorage) *scopedresource.Storage {
	t.Helper()
	s := NewStorage()
	s.Resource.(*genericregistry.Store).Storage.Storage = backend
	if err := s.CompleteWithOptions(&generic.StoreOptions{RESTOptions: generic.RESTOptions{ResourcePrefix: "runners"}}); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *deletionStorage) Get(_ context.Context, key string, _ storage.GetOptions, out runtime.Object) error {
	s.lastKey = key
	s.initial.DeepCopyInto(out.(*ebsv1.Runner))
	return nil
}

func (s *deletionStorage) Watch(_ context.Context, key string, _ storage.ListOptions) (watch.Interface, error) {
	s.lastKey = key
	return watch.NewEmptyWatch(), nil
}

func TestRunnerNativeStorage(t *testing.T) {
	obj := &ebsv1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "runner-a"}}
	backend := &deletionStorage{initial: obj, current: obj}
	s := NewStorage()
	store := s.Resource.(*genericregistry.Store)
	store.Storage.Storage = backend
	destroyed := 0
	store.DestroyFunc = func() { destroyed++ }
	if err := s.CompleteWithOptions(&generic.StoreOptions{RESTOptions: generic.RESTOptions{ResourcePrefix: "native/runners"}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, getter := range []rest.Getter{s.Resource, s.Status.(rest.Getter)} {
		got, err := getter.Get(ctx, obj.Name, &metav1.GetOptions{})
		if err != nil || !reflect.DeepEqual(got, obj) || backend.lastKey != "/native/runners/runner-a" {
			t.Fatalf("get=%v error=%v key=%q", got, err, backend.lastKey)
		}
	}
	w, err := s.Resource.Watch(ctx, &metainternalversion.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.Stop()
	if backend.lastKey != "/native/runners" {
		t.Fatalf("watch key=%q", backend.lastKey)
	}
	s.Status.Destroy()
	if destroyed != 0 {
		t.Fatal("status destroyed shared storage")
	}
	s.Resource.Destroy()
	if destroyed != 1 {
		t.Fatalf("storage destroyed %d times", destroyed)
	}
}

func (s *deletionStorage) GetList(_ context.Context, _ string, _ storage.ListOptions, out runtime.Object) error {
	out.(*ebsv1.RunnerList).Items = []ebsv1.Runner{*s.initial.DeepCopy()}
	return nil
}

func (s *deletionStorage) Delete(ctx context.Context, key string, out runtime.Object, preconditions *storage.Preconditions, validate storage.ValidateObjectFunc, _ runtime.Object) error {
	if err := preconditions.Check(key, s.current); err != nil {
		return err
	}
	if err := validate(ctx, s.current.DeepCopy()); err != nil {
		return err
	}
	s.current.DeepCopyInto(out.(*ebsv1.Runner))
	s.deleted = true
	return nil
}

func TestRunnerDeletion(t *testing.T) {
	for _, mode := range []string{"single", "collection", "dry-run"} {
		for _, phase := range []ebsv1.RunnerPhase{ebsv1.RunnerOffline, ebsv1.RunnerEvicted, ebsv1.RunnerOnline, "", "Unknown"} {
			t.Run(mode+"/"+string(phase), func(t *testing.T) {
				obj := &ebsv1.Runner{
					ObjectMeta: metav1.ObjectMeta{Name: "runner-a", UID: "runner-uid", ResourceVersion: "1"},
					Status:     ebsv1.RunnerStatus{Phase: phase},
				}
				backend := &deletionStorage{initial: obj, current: obj.DeepCopy()}
				r := newDeletionStorage(t, backend).Resource
				options := &metav1.DeleteOptions{}
				if mode == "dry-run" {
					options.DryRun = []string{metav1.DryRunAll}
				}
				var err error
				if mode == "collection" {
					_, err = r.DeleteCollection(context.Background(), nil, options, nil)
				} else {
					_, _, err = r.Delete(context.Background(), obj.Name, nil, options)
				}
				allowed := phase == ebsv1.RunnerOffline || phase == ebsv1.RunnerEvicted
				if (allowed && err != nil) || (!allowed && !apierrors.IsConflict(err)) {
					t.Fatalf("phase=%q error=%v", phase, err)
				}
				if backend.deleted != (allowed && mode != "dry-run") {
					t.Fatalf("deleted=%t", backend.deleted)
				}
			})
		}
	}
}

func TestRunnerDeletionRevalidatesLatestState(t *testing.T) {
	initial := &ebsv1.Runner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-a", UID: "runner-uid", ResourceVersion: "1"},
		Status:     ebsv1.RunnerStatus{Phase: ebsv1.RunnerOffline},
	}
	current := initial.DeepCopy()
	current.ResourceVersion = "2"
	current.Status.Phase = ebsv1.RunnerOnline
	backend := &deletionStorage{initial: initial, current: current}
	r := newDeletionStorage(t, backend).Resource
	_, _, err := r.Delete(context.Background(), initial.Name, nil, &metav1.DeleteOptions{})
	if !apierrors.IsConflict(err) || backend.deleted {
		t.Fatalf("error=%v deleted=%t", err, backend.deleted)
	}
}

func TestRunnerDeletionPreservesCallerValidation(t *testing.T) {
	want := errors.New("admission rejected deletion")
	obj := &ebsv1.Runner{ObjectMeta: metav1.ObjectMeta{Name: "runner-a"}, Status: ebsv1.RunnerStatus{Phase: ebsv1.RunnerOffline}}
	backend := &deletionStorage{initial: obj, current: obj}
	r := newDeletionStorage(t, backend).Resource
	_, _, err := r.Delete(context.Background(), obj.Name, func(context.Context, runtime.Object) error { return want }, nil)
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
}

func TestPrepareForCreateDefaultsOffline(t *testing.T) {
	runner := &ebsv1.Runner{Status: ebsv1.RunnerStatus{Phase: ebsv1.RunnerOnline}}

	NewStorage().Resource.(*genericregistry.Store).CreateStrategy.PrepareForCreate(context.Background(), runner)

	if runner.Status.Phase != ebsv1.RunnerOffline {
		t.Fatalf("status.phase = %q, want Offline", runner.Status.Phase)
	}
}

func TestOrdinaryUpdatePreservesRunnerStatus(t *testing.T) {
	old := &ebsv1.Runner{Status: ebsv1.RunnerStatus{
		Phase: ebsv1.RunnerEvicted, Heartbeat: metav1.NewTime(time.Unix(100, 0)),
	}}
	old.Spec.Type, old.Spec.Arch = "ct", "aarch64"
	for _, test := range []struct {
		name   string
		status ebsv1.RunnerStatus
	}{
		{name: "omitted status"},
		{name: "explicit status", status: ebsv1.RunnerStatus{Phase: ebsv1.RunnerOnline}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := &ebsv1.Runner{Status: test.status}
			candidate.Spec.Unschedulable = true
			candidate.Spec.Type, candidate.Spec.Arch = "vm", "x86_64"
			NewStorage().Resource.(*genericregistry.Store).UpdateStrategy.PrepareForUpdate(context.Background(), candidate, old)
			if !reflect.DeepEqual(candidate.Status, old.Status) || !candidate.Spec.Unschedulable {
				t.Fatalf("ordinary update did not preserve status and apply spec: %+v", candidate)
			}
			if candidate.Spec.Type != old.Spec.Type || candidate.Spec.Arch != old.Spec.Arch {
				t.Fatalf("ordinary update changed type or arch: %+v", candidate.Spec)
			}
		})
	}
}

func TestStatusUpdateKeepsRequestedRunnerPhase(t *testing.T) {
	for _, phase := range []ebsv1.RunnerPhase{ebsv1.RunnerOnline, ebsv1.RunnerEvicted, ebsv1.RunnerOffline} {
		t.Run(string(phase), func(t *testing.T) {
			old := &ebsv1.Runner{}
			old.Spec.Unschedulable = true
			wanted := ebsv1.RunnerStatus{Phase: phase, Heartbeat: metav1.NewTime(time.Unix(200, 0))}
			candidate := &ebsv1.Runner{Status: wanted}
			s := NewStorage().Status.(*genericregistry.Store).UpdateStrategy
			s.PrepareForUpdate(context.Background(), candidate, old)
			if !reflect.DeepEqual(candidate.Status, wanted) || !reflect.DeepEqual(candidate.Spec, old.Spec) {
				t.Fatalf("status update did not apply status and preserve spec: %+v", candidate)
			}
			if errs := s.ValidateUpdate(context.Background(), candidate, old); len(errs) != 0 {
				t.Fatalf("status update failed: %v", errs)
			}
		})
	}
}

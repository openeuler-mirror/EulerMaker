package job

import (
	"context"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/registry/ebs/oldjob"
)

type lifecycleMemory struct {
	storage.Interface
	mu            sync.Mutex
	job           *ebsv1.Job
	revision      int
	deleteFailure error
}

func (*lifecycleMemory) Versioner() storage.Versioner { return storage.APIObjectVersioner{} }

func (s *lifecycleMemory) Get(_ context.Context, key string, _ storage.GetOptions, out runtime.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return storage.NewKeyNotFoundError(key, 0)
	}
	s.job.DeepCopyInto(out.(*ebsv1.Job))
	return nil
}

func (s *lifecycleMemory) GuaranteedUpdate(_ context.Context, key string, out runtime.Object, _ bool,
	pre *storage.Preconditions, update storage.UpdateFunc, _ runtime.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return storage.NewKeyNotFoundError(key, 0)
	}
	if err := pre.Check(key, s.job); err != nil {
		return err
	}
	obj, _, err := update(s.job.DeepCopy(), storage.ResponseMeta{})
	if err != nil {
		return err
	}
	s.revision++
	s.job = obj.(*ebsv1.Job).DeepCopy()
	s.job.ResourceVersion = fmt.Sprint(s.revision)
	s.job.DeepCopyInto(out.(*ebsv1.Job))
	return nil
}

func (s *lifecycleMemory) Delete(_ context.Context, key string, out runtime.Object, pre *storage.Preconditions,
	validate storage.ValidateObjectFunc, _ runtime.Object) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return storage.NewKeyNotFoundError(key, 0)
	}
	if err := pre.Check(key, s.job); err != nil {
		return err
	}
	if err := validate(context.Background(), s.job); err != nil {
		return err
	}
	if s.deleteFailure != nil {
		return s.deleteFailure
	}
	s.job.DeepCopyInto(out.(*ebsv1.Job))
	s.job = nil
	return nil
}

type lifecycleArchive struct {
	job     *ebsv1.Job
	failure error
}

func (s *lifecycleArchive) Archive(_ context.Context, job *ebsv1.Job) (*ebsv1.Job, error) {
	if s.failure != nil {
		return nil, s.failure
	}
	s.job = job.DeepCopy()
	s.job.DeletionTimestamp = nil
	s.job.Finalizers = []string{ebsv1.JobCleanupFinalizer}
	s.job.ResourceVersion = "v1:1:1"
	return s.job.DeepCopy(), nil
}

func (s *lifecycleArchive) ArchiveState(context.Context, string) (*ebsv1.JobArchiveState, error) {
	if s.job == nil {
		return nil, apierrors.NewNotFound(ebsv1.Resource("oldjobs"), "job")
	}
	digest, err := oldjob.SnapshotDigest(s.job)
	if err != nil {
		return nil, err
	}
	action := "retain"
	if s.job.DeletionTimestamp != nil {
		action = "delete"
	}
	return &ebsv1.JobArchiveState{UID: string(s.job.UID), Digest: digest, Action: action, Phase: "Stored"}, nil
}

func (s *lifecycleArchive) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	if s.job == nil {
		return nil, apierrors.NewNotFound(ebsv1.Resource("oldjobs"), "job")
	}
	return s.job.DeepCopy(), nil
}

func (s *lifecycleArchive) Finalize(context.Context, string, *metav1.Preconditions) (*ebsv1.Job, error) {
	out := s.job.DeepCopy()
	s.job = nil
	return out, nil
}

type archiveBuildGetter struct{ deleting bool }

func (g *archiveBuildGetter) Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error) {
	build := &ebsv1.Build{}
	if g.deleting {
		now := metav1.Now()
		build.DeletionTimestamp = &now
	}
	return build, nil
}

func newLifecycleTest(t *testing.T) (*Lifecycle, *lifecycleMemory, *lifecycleArchive, *archiveBuildGetter) {
	t.Helper()
	s := NewStorage()
	// The lifecycle exercises the same native storage boundary as etcd, including UID/resourceVersion CAS.
	store := s.Resource.(*genericregistry.Store)
	memory := &lifecycleMemory{revision: 10, job: &ebsv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "project", UID: "uid", ResourceVersion: "10",
			Labels: map[string]string{ebsv1.JobBuildNameLabel: "build"}, Finalizers: []string{ebsv1.JobArchiveFinalizer}},
		Status: ebsv1.JobStatus{Phase: ebsv1.JobSucceeded, EndTime: metav1.NewTime(time.Unix(100, 0))},
	}}
	store.Storage.Storage = memory
	scheme := runtime.NewScheme()
	if err := ebsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store.Storage.Codec = serializer.NewCodecFactory(scheme).LegacyCodec(ebsv1.SchemeGroupVersion)
	store.KeyFunc = func(context.Context, string) (string, error) { return "/jobs/project/job", nil }
	archive, builds := &lifecycleArchive{}, &archiveBuildGetter{}
	return NewLifecycle(store, archive, builds), memory, archive, builds
}

func archivePre(job *ebsv1.Job) *metav1.Preconditions {
	uid, rv := job.UID, job.ResourceVersion
	return &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}
}

func TestJobArchiveLifecycle(t *testing.T) {
	ctx := context.Background()
	lifecycle, memory, archived, _ := newLifecycleTest(t)
	old, err := lifecycle.Archive(ctx, "job", archivePre(memory.job))
	if err != nil {
		t.Fatal(err)
	}
	if old.ResourceVersion != "v1:1:1" || memory.job.Annotations[ebsv1.JobArchiveActionAnnotation] != "retain" || memory.job.DeletionTimestamp == nil {
		t.Fatalf("source=%+v archive=%+v", memory.job, old)
	}
	if _, err := lifecycle.FinalizeOldJob(ctx, "job", archivePre(old)); !apierrors.IsConflict(err) {
		t.Fatalf("premature cleanup: %v", err)
	}
	if _, err := lifecycle.Finalize(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
	state, err := lifecycle.ArchiveState(ctx, "job")
	if err != nil || state.Phase != "Transferred" || archived.job == nil || memory.job != nil {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	now := metav1.Now()
	archived.job.DeletionTimestamp = &now
	if _, err := lifecycle.FinalizeOldJob(ctx, "job", archivePre(archived.job)); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.ArchiveState(ctx, "job"); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted migration confirmation: %v", err)
	}
}

func TestFinalizeResumesAfterPhysicalDeleteFailure(t *testing.T) {
	for _, action := range []string{"retain", "delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			lifecycle, memory, archived, _ := newLifecycleTest(t)
			if action == "delete" {
				if _, _, err := lifecycle.Delete(ctx, "job", nil, &metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); err != nil {
				t.Fatal(err)
			}
			if action == "delete" {
				now := metav1.Now()
				archived.job.DeletionTimestamp = &now
			}
			stale := archivePre(memory.job)
			memory.deleteFailure = fmt.Errorf("etcd delete unavailable")
			if _, err := lifecycle.Finalize(ctx, "job", stale); err == nil {
				t.Fatal("expected physical delete failure")
			}
			if memory.job == nil || len(memory.job.Finalizers) != 0 || memory.job.ResourceVersion == *stale.ResourceVersion {
				t.Fatalf("finalizer removal was not persisted: %+v", memory.job)
			}
			state, err := lifecycle.ArchiveState(ctx, "job")
			if err != nil || state.Phase != "Stored" {
				t.Fatalf("unfinished transfer: state=%+v err=%v", state, err)
			}
			memory.deleteFailure = nil
			if _, err := lifecycle.Finalize(ctx, "job", stale); err == nil {
				t.Fatal("stale preconditions accepted")
			}
			if _, err := lifecycle.Finalize(ctx, "job", archivePre(memory.job)); err != nil {
				t.Fatal(err)
			}
			state, err = lifecycle.ArchiveState(ctx, "job")
			if err != nil || state.Phase != "Transferred" || memory.job != nil || archived.job == nil {
				t.Fatalf("resumed transfer: state=%+v err=%v", state, err)
			}
		})
	}
}

func TestArchiveConfirmationMatchesSnapshot(t *testing.T) {
	ctx := context.Background()
	lifecycle, memory, archived, _ := newLifecycleTest(t)
	if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
	message := archived.job.Status.Message
	archived.job.Status.Message = "different-snapshot"
	if _, err := lifecycle.ArchiveState(ctx, "job"); !apierrors.IsConflict(err) {
		t.Fatalf("mismatched snapshot confirmation: %v", err)
	}
	if _, err := lifecycle.Finalize(ctx, "job", archivePre(memory.job)); !apierrors.IsConflict(err) {
		t.Fatalf("mismatched snapshot finalization: %v", err)
	}
	if memory.job == nil || !slices.Contains(memory.job.Finalizers, ebsv1.JobArchiveFinalizer) {
		t.Fatal("unconfirmed source lost its protection")
	}
	archived.job.Status.Message = message
	if _, err := lifecycle.Finalize(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
}

func TestJobDeleteDryRunAndForeignFinalizer(t *testing.T) {
	ctx := context.Background()
	lifecycle, memory, archive, _ := newLifecycleTest(t)
	if obj, deleted, err := lifecycle.Delete(ctx, "job", nil, &metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}}); err != nil || deleted || obj.(*ebsv1.Job).DeletionTimestamp == nil {
		t.Fatalf("dry-run: %v deleted=%v", err, deleted)
	}
	if memory.job.DeletionTimestamp != nil || memory.revision != 10 {
		t.Fatal("dry-run persisted intent")
	}
	memory.job.Finalizers = append(memory.job.Finalizers, "example.test/other")
	if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
	if _, err := lifecycle.Finalize(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
	if memory.job == nil || !slices.Equal(memory.job.Finalizers, []string{"example.test/other"}) {
		t.Fatal("other finalizer was bypassed")
	}
	state, err := lifecycle.ArchiveState(ctx, "job")
	if err != nil || state.Phase != "Stored" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if _, err := lifecycle.FinalizeOldJob(ctx, "job", archivePre(archive.job)); !apierrors.IsConflict(err) {
		t.Fatalf("source still exists: %v", err)
	}
}

func TestDeleteUpgradesRetainBeforeHandoff(t *testing.T) {
	ctx := context.Background()
	lifecycle, memory, archived, _ := newLifecycleTest(t)
	if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
	stale := archivePre(memory.job)
	if _, deleted, err := lifecycle.Delete(ctx, "job", nil, &metav1.DeleteOptions{Preconditions: stale}); err != nil || deleted {
		t.Fatalf("delete: %v %v", deleted, err)
	}
	if _, err := lifecycle.Finalize(ctx, "job", stale); err == nil {
		t.Fatal("stale retain decision completed")
	}
	if _, err := lifecycle.Finalize(ctx, "job", archivePre(memory.job)); !apierrors.IsConflict(err) {
		t.Fatalf("missing handoff accepted: %v", err)
	}
	now := metav1.Now()
	archived.job.DeletionTimestamp = &now
	if _, err := lifecycle.Finalize(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
	if memory.job != nil {
		t.Fatal("source was not removed")
	}
}

func TestDeletingJobCanArchiveWhileParentDeleting(t *testing.T) {
	ctx := context.Background()
	lifecycle, memory, archived, builds := newLifecycleTest(t)
	builds.deleting = true
	if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); !apierrors.IsConflict(err) {
		t.Fatalf("new retain: %v", err)
	}
	memory.job.Status.Phase = ebsv1.JobRunning
	if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); !apierrors.IsConflict(err) {
		t.Fatalf("running archive: %v", err)
	}
	// The caller aborts the Job before requesting deletion.
	memory.job.Status.Phase = ebsv1.JobAborted
	if _, _, err := lifecycle.Delete(ctx, "job", nil, &metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	archived.failure = fmt.Errorf("ES unavailable")
	if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); err == nil {
		t.Fatal("expected ES failure")
	}
	if !slices.Contains(memory.job.Finalizers, ebsv1.JobArchiveFinalizer) || memory.job.DeletionTimestamp == nil || !memory.job.Status.Phase.IsTerminal() {
		t.Fatal("frozen source lost")
	}
	archived.failure = nil
	if _, err := lifecycle.Archive(ctx, "job", archivePre(memory.job)); err != nil {
		t.Fatal(err)
	}
	state, err := lifecycle.ArchiveState(ctx, "job")
	if err != nil || state.Action != "delete" {
		t.Fatalf("delete intent confirmation=%+v err=%v", state, err)
	}
}

func TestJobDeletionRequiresTerminalPhase(t *testing.T) {
	for _, phase := range []ebsv1.JobPhase{"", "Unknown", ebsv1.JobPending, ebsv1.JobRunning,
		ebsv1.JobSucceeded, ebsv1.JobFailed, ebsv1.JobAborted} {
		t.Run(string(phase), func(t *testing.T) {
			for _, dryRun := range []bool{false, true} {
				lifecycle, memory, _, _ := newLifecycleTest(t)
				memory.job.Status.Phase = phase
				opts := &metav1.DeleteOptions{Preconditions: archivePre(memory.job)}
				if dryRun {
					opts.DryRun = []string{metav1.DryRunAll}
				}
				_, deleted, err := lifecycle.Delete(context.Background(), "job", nil, opts)
				if phase.IsTerminal() {
					if err != nil || deleted {
						t.Fatalf("terminal delete: deleted=%v err=%v", deleted, err)
					}
				} else if !apierrors.IsConflict(err) {
					t.Fatalf("nonterminal delete: %v", err)
				}
				if memory.job.Status.Phase != phase {
					t.Fatal("DELETE changed Job status")
				}
				if !phase.IsTerminal() || dryRun {
					if memory.job.DeletionTimestamp != nil || memory.revision != 10 || len(memory.job.Annotations) != 0 {
						t.Fatal("rejected or dry-run delete changed persisted Job")
					}
				}
			}
		})
	}
}

func TestArchiveHTTPPreconditions(t *testing.T) {
	lifecycle, _, _, _ := newLifecycleTest(t)
	for _, body := range []string{`{}`, `{"preconditions":{"uid":"uid"}}`, `{} {}`} {
		response := &abortResponse{}
		h, err := NewArchiveStorage(lifecycle).(rest.Connecter).Connect(context.Background(), "job", nil, response)
		if err != nil {
			t.Fatal(err)
		}
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(body)))
		if !apierrors.IsBadRequest(response.err) {
			t.Fatalf("body=%s err=%v", body, response.err)
		}
	}
}

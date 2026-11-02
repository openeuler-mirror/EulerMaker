package job

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/storage"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/esstore"
)

type lifecycleMemory struct {
	storage.Interface
	mu       sync.Mutex
	job      *ebsv1.Job
	revision int
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
	s.job.DeepCopyInto(out.(*ebsv1.Job))
	s.job = nil
	return nil
}

type lifecycleArchive struct {
	job     *ebsv1.Job
	state   *ebsv1.JobArchiveState
	failure error
}

func (s *lifecycleArchive) Archive(_ context.Context, job *ebsv1.Job) (*ebsv1.Job, error) {
	if s.failure != nil {
		return nil, s.failure
	}
	s.job = job.DeepCopy()
	s.job.DeletionTimestamp = nil
	s.job.Finalizers = []string{ebsv1.JobArtifactsCleanupFinalizer}
	s.job.ResourceVersion = "v1:1:1"
	digest, err := esstore.JobArchiveDigest(job)
	if err != nil {
		return nil, err
	}
	s.state = &ebsv1.JobArchiveState{UID: string(job.UID), Action: job.Annotations[ebsv1.JobArchiveActionAnnotation],
		Digest: digest, Phase: "Stored"}
	return s.job.DeepCopy(), nil
}

func (s *lifecycleArchive) ArchiveState(context.Context, string) (*ebsv1.JobArchiveState, error) {
	if s.state == nil {
		return nil, apierrors.NewNotFound(ebsv1.Resource("oldjobs"), "job")
	}
	state := *s.state
	return &state, nil
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
}

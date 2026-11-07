package oldjob

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/util/dryrun"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "oldjobs", Singular: "oldjob", Kind: "Job",
		New:     func() runtime.Object { return &ebsv1.Job{} },
		NewList: func() runtime.Object { return &ebsv1.JobList{} },
	})
}

// Storage stores immutable snapshots with an independent deletion lifecycle.
type Storage struct{ store *esstore.Store }

func NewLifecycle(store *esstore.Store) *Storage {
	return &Storage{store: store}
}

func (s *Storage) New() runtime.Object     { return &ebsv1.Job{} }
func (s *Storage) NewList() runtime.Object { return &ebsv1.JobList{} }
func (s *Storage) Destroy()                {}
func (s *Storage) NamespaceScoped() bool   { return true }
func (s *Storage) GetSingularName() string { return "oldjob" }

func (s *Storage) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

func (s *Storage) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	if options != nil && options.ResourceVersion != "" {
		return nil, apierrors.NewBadRequest("oldjobs does not support resourceVersion reads")
	}
	return s.store.Get(ctx, name, options)
}

func (s *Storage) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	if options != nil && (options.Watch || options.ResourceVersion != "" || options.ResourceVersionMatch != "") {
		return nil, apierrors.NewBadRequest("oldjobs does not support watch or resourceVersion reads")
	}
	return s.store.List(ctx, options)
}

func (s *Storage) read(ctx context.Context, name string) (*ebsv1.Job, error) {
	if err := oldJobIdentity(ctx, name); err != nil {
		return nil, err
	}
	obj, err := s.store.Get(esstore.WithSourceFilter(ctx, es.SourceFilter{}), name, nil)
	if err != nil {
		return nil, err
	}
	return obj.(*ebsv1.Job), nil
}

func oldJobIdentity(ctx context.Context, name string) error {
	if request.NamespaceValue(ctx) == "" || name == "" {
		return apierrors.NewBadRequest("project and name are required")
	}
	return nil
}

// Delete records deletion intent without contacting Artifact Manager.
func (s *Storage) Delete(ctx context.Context, name string, admission rest.ValidateObjectFunc, opts *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if err := oldJobIdentity(ctx, name); err != nil {
		return nil, false, err
	}
	if opts == nil {
		opts = &metav1.DeleteOptions{}
	}
	obj, err := s.store.UpdateLifecycle(ctx, name, opts.Preconditions, dryrun.IsDryRun(opts.DryRun), func(obj runtime.Object) (bool, error) {
		job := obj.(*ebsv1.Job)
		if admission != nil {
			if err := admission(ctx, job.DeepCopy()); err != nil {
				return false, err
			}
		}
		if job.DeletionTimestamp != nil {
			return false, nil
		}
		now := metav1.Now()
		job.DeletionTimestamp = &now
		if !slices.Contains(job.Finalizers, ebsv1.JobCleanupFinalizer) {
			job.Finalizers = append(job.Finalizers, ebsv1.JobCleanupFinalizer)
		}
		return true, nil
	})
	return obj, false, err
}

// Finalize removes the cleanup finalizer and deletes the document once all finalizers are gone.
// The caller guarantees all archive writes and retries have stopped before cleanup.
func (s *Storage) Finalize(ctx context.Context, name string, pre *metav1.Preconditions) (*ebsv1.Job, error) {
	if err := oldJobIdentity(ctx, name); err != nil {
		return nil, err
	}
	obj, err := s.store.UpdateLifecycle(ctx, name, pre, false, func(obj runtime.Object) (bool, error) {
		job := obj.(*ebsv1.Job)
		if job.DeletionTimestamp == nil {
			return false, apierrors.NewConflict(ebsv1.Resource("oldjobs"), name, fmt.Errorf("OldJob is not deleting"))
		}
		job.Finalizers = slices.DeleteFunc(job.Finalizers, func(value string) bool { return value == ebsv1.JobCleanupFinalizer })
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return obj.(*ebsv1.Job), nil
}

func jobArchiveSnapshot(job *ebsv1.Job) *ebsv1.Job {
	next := job.DeepCopy()
	next.TypeMeta = metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"}
	next.ResourceVersion, next.Finalizers, next.ManagedFields = "", nil, nil
	next.DeletionTimestamp, next.DeletionGracePeriodSeconds = nil, nil
	delete(next.Annotations, ebsv1.JobArchiveActionAnnotation)
	return next
}

// SnapshotDigest identifies business content independently of lifecycle metadata and storage versions.
func SnapshotDigest(job *ebsv1.Job) (string, error) {
	data, err := json.Marshal(jobArchiveSnapshot(job))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// Archive creates or confirms a frozen snapshot without overwriting its deletion lifecycle.
func (s *Storage) Archive(ctx context.Context, job *ebsv1.Job) (*ebsv1.Job, error) {
	action := job.Annotations[ebsv1.JobArchiveActionAnnotation]
	if job.DeletionTimestamp == nil || (action != "retain" && action != "delete") ||
		!job.Status.Phase.IsTerminal() || job.Status.EndTime.IsZero() {
		return nil, apierrors.NewConflict(ebsv1.Resource("oldjobs"), job.Name, fmt.Errorf("Job snapshot has not been frozen"))
	}
	digest, err := SnapshotDigest(job)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	next := jobArchiveSnapshot(job)
	next.Finalizers = []string{ebsv1.JobCleanupFinalizer}
	if err := s.store.CreateSnapshot(ctx, next); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	// A real-time GET confirms the snapshot without replaying an update.
	stored, err := s.read(ctx, job.Name)
	if err != nil {
		return nil, err
	}
	storedDigest, err := SnapshotDigest(stored)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	if stored.UID != job.UID || storedDigest != digest {
		return nil, apierrors.NewConflict(ebsv1.Resource("oldjobs"), job.Name, fmt.Errorf("archive identity or frozen snapshot conflicts"))
	}
	return stored, nil
}

// ArchiveState derives confirmation from the stored Job; a deleted OldJob returns NotFound.
func (s *Storage) ArchiveState(ctx context.Context, name string) (*ebsv1.JobArchiveState, error) {
	job, err := s.read(ctx, name)
	if err != nil {
		return nil, err
	}
	digest, err := SnapshotDigest(job)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	action := "retain"
	if job.DeletionTimestamp != nil {
		action = "delete"
	}
	return &ebsv1.JobArchiveState{UID: string(job.UID), Action: action, Digest: digest, Phase: "Stored"}, nil
}

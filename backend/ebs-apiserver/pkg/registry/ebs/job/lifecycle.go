package job

import (
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storage"
	storeerr "k8s.io/apiserver/pkg/storage/errors"
	"k8s.io/apiserver/pkg/util/dryrun"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/esstore"
)

type ArchiveStore interface {
	rest.Getter
	Archive(context.Context, *ebsv1.Job) (*ebsv1.Job, error)
	ArchiveState(context.Context, string) (*ebsv1.JobArchiveState, error)
	Finalize(context.Context, string, *metav1.Preconditions) (*ebsv1.Job, error)
}

// Lifecycle atomically records intent and freezes the source in the same etcd object.
// The immutable ES snapshot and its deletion fence survive removal of this source.
type Lifecycle struct {
	*genericregistry.Store
	oldJobs ArchiveStore
	builds  rest.Getter
}

func NewLifecycle(store *genericregistry.Store, oldJobs ArchiveStore, builds rest.Getter) *Lifecycle {
	return &Lifecycle{Store: store, oldJobs: oldJobs, builds: builds}
}

func (s *Lifecycle) update(ctx context.Context, name string, pre *metav1.Preconditions, dryRun bool,
	change func(*ebsv1.Job) error) (*ebsv1.Job, error) {
	key, err := s.KeyFunc(ctx, name)
	if err != nil {
		return nil, err
	}
	var conditions *storage.Preconditions
	if pre != nil {
		conditions = &storage.Preconditions{UID: pre.UID, ResourceVersion: pre.ResourceVersion}
	}
	out := &ebsv1.Job{}
	err = s.Storage.GuaranteedUpdate(ctx, key, out, false, conditions, func(obj runtime.Object, _ storage.ResponseMeta) (runtime.Object, *uint64, error) {
		job := obj.(*ebsv1.Job).DeepCopy()
		if err := change(job); err != nil {
			return nil, nil, err
		}
		return job, nil, nil
	}, dryRun, nil)
	if err != nil {
		return nil, storeerr.InterpretUpdateError(err, ebsv1.Resource("jobs"), name)
	}
	return out, nil
}

func markDeleting(job *ebsv1.Job, action string) {
	if job.Annotations == nil {
		job.Annotations = make(map[string]string)
	}
	if job.Annotations[ebsv1.JobArchiveActionAnnotation] != "delete" {
		job.Annotations[ebsv1.JobArchiveActionAnnotation] = action
	}
	if !slices.Contains(job.Finalizers, ebsv1.JobArchiveFinalizer) {
		job.Finalizers = append(job.Finalizers, ebsv1.JobArchiveFinalizer)
	}
	if job.DeletionTimestamp == nil {
		now, zero := metav1.Now(), int64(0)
		job.DeletionTimestamp, job.DeletionGracePeriodSeconds = &now, &zero
	}
}

func (s *Lifecycle) Delete(ctx context.Context, name string, admission rest.ValidateObjectFunc, opts *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if opts == nil {
		opts = &metav1.DeleteOptions{}
	}
	out, err := s.update(ctx, name, opts.Preconditions, dryrun.IsDryRun(opts.DryRun), func(job *ebsv1.Job) error {
		if !job.Status.Phase.IsTerminal() {
			return apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("only terminal Jobs can be deleted; abort the Job before deleting it"))
		}
		if admission != nil {
			if err := admission(ctx, job.DeepCopy()); err != nil {
				return err
			}
		}
		markDeleting(job, "delete")
		return nil
	})
	return out, false, err
}

func (s *Lifecycle) DeleteCollection(ctx context.Context, admission rest.ValidateObjectFunc, opts *metav1.DeleteOptions,
	listOptions *internalversion.ListOptions) (runtime.Object, error) {
	obj, err := s.List(ctx, listOptions)
	if err != nil {
		return nil, err
	}
	for _, job := range obj.(*ebsv1.JobList).Items {
		if _, _, err := s.Delete(ctx, job.Name, admission, opts); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return obj, nil
}

func (s *Lifecycle) Archive(ctx context.Context, name string, pre *metav1.Preconditions) (*ebsv1.Job, error) {
	obj, err := s.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	current := obj.(*ebsv1.Job)
	if current.UID != *pre.UID || current.ResourceVersion != *pre.ResourceVersion {
		return nil, apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("Job preconditions failed"))
	}
	if current.Annotations[ebsv1.JobArchiveActionAnnotation] == "" {
		if current.DeletionTimestamp != nil {
			return nil, apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("deletion intent is missing"))
		}
		buildName := current.Labels[ebsv1.JobBuildNameLabel]
		if buildName == "" {
			return nil, apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("parent Build is missing"))
		}
		build, err := s.builds.Get(ctx, buildName, &metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if build.(*ebsv1.Build).DeletionTimestamp != nil {
			return nil, apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("parent Build is deleting"))
		}
	}
	frozen, err := s.update(ctx, name, pre, false, func(job *ebsv1.Job) error {
		if !job.Status.Phase.IsTerminal() || job.Status.EndTime.IsZero() {
			return apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("archive requires a terminal Job with endTime"))
		}
		markDeleting(job, "retain")
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.oldJobs.Archive(ctx, frozen)
}

func (s *Lifecycle) ArchiveState(ctx context.Context, name string) (*ebsv1.JobArchiveState, error) {
	state, err := s.oldJobs.ArchiveState(ctx, name)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	obj, getErr := s.Get(ctx, name, &metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) && err == nil {
		state.Phase = "Transferred"
		return state, nil
	}
	if getErr != nil {
		return nil, getErr
	}
	job := obj.(*ebsv1.Job)
	action := job.Annotations[ebsv1.JobArchiveActionAnnotation]
	if action == "" {
		return nil, apierrors.NewNotFound(ebsv1.Resource("jobs/archive"), name)
	}
	if state == nil {
		state = &ebsv1.JobArchiveState{UID: string(job.UID), Phase: "Pending"}
	}
	if state.UID != string(job.UID) {
		return nil, apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("archive UID mismatch"))
	}
	if state.Phase == "Stored" {
		if err := confirmArchive(job, state); err != nil {
			return nil, err
		}
	}
	state.Action = action
	return state, nil
}

func (s *Lifecycle) Finalize(ctx context.Context, name string, pre *metav1.Preconditions) (*ebsv1.Job, error) {
	state, err := s.oldJobs.ArchiveState(ctx, name)
	if err != nil {
		return nil, err
	}
	obj, err := s.oldJobs.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	archived := obj.(*ebsv1.Job)
	out, err := s.update(ctx, name, pre, false, func(job *ebsv1.Job) error {
		if archived.UID != job.UID {
			return apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("archive has not been confirmed"))
		}
		if err := confirmArchive(job, state); err != nil {
			return err
		}
		if job.Annotations[ebsv1.JobArchiveActionAnnotation] == "delete" && archived.DeletionTimestamp == nil {
			return apierrors.NewConflict(ebsv1.Resource("jobs"), name, fmt.Errorf("OldJob deletion intent has not been recorded"))
		}
		job.Finalizers = slices.DeleteFunc(job.Finalizers, func(value string) bool { return value == ebsv1.JobArchiveFinalizer })
		return nil
	})
	if err != nil || len(out.Finalizers) != 0 {
		return out, err
	}
	key, err := s.KeyFunc(ctx, name)
	if err != nil {
		return nil, err
	}
	conditions := &storage.Preconditions{UID: &out.UID, ResourceVersion: &out.ResourceVersion}
	err = s.Storage.Delete(ctx, key, out, conditions, rest.ValidateAllObjectFunc, false, nil)
	if err != nil {
		return nil, storeerr.InterpretDeleteError(err, ebsv1.Resource("jobs"), name)
	}
	return out, nil
}

func confirmArchive(job *ebsv1.Job, state *ebsv1.JobArchiveState) error {
	digest, err := esstore.JobArchiveDigest(job)
	if err != nil {
		return apierrors.NewInternalError(err)
	}
	if job.DeletionTimestamp == nil || !job.Status.Phase.IsTerminal() || job.Status.EndTime.IsZero() ||
		state.UID != string(job.UID) || state.Digest != digest {
		return apierrors.NewConflict(ebsv1.Resource("jobs"), job.Name, fmt.Errorf("archive identity or frozen snapshot conflicts"))
	}
	return nil
}

func (s *Lifecycle) FinalizeOldJob(ctx context.Context, name string, pre *metav1.Preconditions) (*ebsv1.Job, error) {
	state, err := s.ArchiveState(ctx, name)
	if err != nil {
		return nil, err
	}
	if state.UID != string(*pre.UID) || state.Phase != "Transferred" {
		return nil, apierrors.NewConflict(ebsv1.Resource("oldjobs"), name, fmt.Errorf("source Job has not finished migration"))
	}
	return s.oldJobs.Finalize(ctx, name, pre)
}

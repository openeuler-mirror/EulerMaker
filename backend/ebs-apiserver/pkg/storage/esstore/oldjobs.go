package esstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/util/dryrun"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
)

// OldJobs stores immutable snapshots with an independent deletion lifecycle.
type OldJobs struct{ store *Store }

func NewOldJobs(client *es.Client, template *genericregistry.Store) *OldJobs {
	s := New(client, "oldjob", "Job", template)
	s.resource, s.singular = ebsv1.Resource("oldjobs"), ebsv1.Resource("oldjob")
	return &OldJobs{store: s}
}

func (s *OldJobs) New() runtime.Object     { return &ebsv1.Job{} }
func (s *OldJobs) NewList() runtime.Object { return &ebsv1.JobList{} }
func (s *OldJobs) Destroy()                {}
func (s *OldJobs) NamespaceScoped() bool   { return true }
func (s *OldJobs) GetSingularName() string { return "oldjob" }

func (s *OldJobs) ConvertToTable(ctx context.Context, obj runtime.Object, opts runtime.Object) (*metav1.Table, error) {
	return s.store.ConvertToTable(ctx, obj, opts)
}

func (s *OldJobs) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	if options != nil && options.ResourceVersion != "" {
		return nil, apierrors.NewBadRequest("oldjobs does not support resourceVersion reads")
	}
	return s.store.Get(ctx, name, options)
}

func (s *OldJobs) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	if options != nil && (options.Watch || options.ResourceVersion != "" || options.ResourceVersionMatch != "") {
		return nil, apierrors.NewBadRequest("oldjobs does not support watch or resourceVersion reads")
	}
	return s.store.List(ctx, options)
}

func (s *OldJobs) read(ctx context.Context, name string, pre *metav1.Preconditions) (*es.Hit, *ebsv1.Job, error) {
	if request.NamespaceValue(ctx) == "" || name == "" {
		return nil, nil, apierrors.NewBadRequest("project and name are required")
	}
	hit, err := s.store.client.Get(ctx, "oldjob", s.store.documentID(ctx, name))
	if err != nil {
		return nil, nil, s.store.apiError(err, name)
	}
	obj, err := s.store.objectFromHit(hit)
	if err != nil {
		return nil, nil, err
	}
	job := obj.(*ebsv1.Job)
	if pre != nil && (pre.UID != nil && *pre.UID != job.UID || pre.ResourceVersion != nil && *pre.ResourceVersion != job.ResourceVersion) {
		return nil, nil, apierrors.NewConflict(s.store.resource, name, fmt.Errorf("archived Job preconditions failed"))
	}
	return hit, job, nil
}

// Delete records deletion intent without contacting Artifact Manager.
func (s *OldJobs) Delete(ctx context.Context, name string, admission rest.ValidateObjectFunc, opts *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if opts == nil {
		opts = &metav1.DeleteOptions{}
	}
	hit, job, err := s.read(ctx, name, opts.Preconditions)
	if err != nil {
		return nil, false, err
	}
	if admission != nil {
		if err := admission(ctx, job.DeepCopy()); err != nil {
			return nil, false, err
		}
	}
	if job.DeletionTimestamp != nil {
		return job, false, nil
	}
	now := metav1.Now()
	job.DeletionTimestamp = &now
	if !slices.Contains(job.Finalizers, ebsv1.JobArtifactsCleanupFinalizer) {
		job.Finalizers = append(job.Finalizers, ebsv1.JobArtifactsCleanupFinalizer)
	}
	if dryrun.IsDryRun(opts.DryRun) {
		return job, false, nil
	}
	if hit.Document.Archive == nil {
		return nil, false, apierrors.NewInternalError(fmt.Errorf("archive confirmation is missing"))
	}
	hit.Document.Archive.Action = "delete"
	result, err := s.save(ctx, hit, job, false)
	return result, false, err
}

// Finalize removes only the cleanup finalizer, retaining a lightweight fence against delayed archive writes.
func (s *OldJobs) Finalize(ctx context.Context, name string, pre *metav1.Preconditions) (*ebsv1.Job, error) {
	hit, job, err := s.read(ctx, name, pre)
	if err != nil {
		return nil, err
	}
	if job.DeletionTimestamp == nil {
		return nil, apierrors.NewConflict(s.store.resource, name, fmt.Errorf("OldJob is not deleting"))
	}
	job.Finalizers = slices.DeleteFunc(job.Finalizers, func(value string) bool { return value == ebsv1.JobArtifactsCleanupFinalizer })
	return s.save(ctx, hit, job, len(job.Finalizers) == 0)
}

func (s *OldJobs) save(ctx context.Context, hit *es.Hit, job *ebsv1.Job, deleted bool) (*ebsv1.Job, error) {
	doc, id, err := s.store.documentFor(ctx, job)
	if err != nil {
		return nil, err
	}
	doc.ArchivedAt, doc.Archive = hit.Document.ArchivedAt, hit.Document.Archive
	if doc.Archive == nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("archive confirmation is missing"))
	}
	if deleted {
		doc.Archive.Deleted = true
		doc.Data, doc.Metadata.Labels = nil, nil
	}
	version, err := s.store.client.Update(ctx, "oldjob", id, doc, hit.SeqNo, hit.PrimaryTerm)
	if err != nil {
		return nil, s.store.apiError(err, job.Name)
	}
	job.ResourceVersion = encodeVersion(version.SeqNo, version.PrimaryTerm)
	return job, nil
}

func jobArchiveSnapshot(job *ebsv1.Job) *ebsv1.Job {
	next := job.DeepCopy()
	next.TypeMeta = metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"}
	next.ResourceVersion, next.Finalizers, next.ManagedFields = "", nil, nil
	next.DeletionTimestamp, next.DeletionGracePeriodSeconds = nil, nil
	delete(next.Annotations, ebsv1.JobArchiveActionAnnotation)
	return next
}

// JobArchiveDigest identifies business content independently of lifecycle metadata and storage versions.
func JobArchiveDigest(job *ebsv1.Job) (string, error) {
	data, err := json.Marshal(jobArchiveSnapshot(job))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// Archive can only create or confirm a frozen snapshot; it never overwrites an existing lifecycle or fence.
func (s *OldJobs) Archive(ctx context.Context, job *ebsv1.Job) (*ebsv1.Job, error) {
	action := job.Annotations[ebsv1.JobArchiveActionAnnotation]
	if job.DeletionTimestamp == nil || (action != "retain" && action != "delete") ||
		!job.Status.Phase.IsTerminal() || job.Status.EndTime.IsZero() {
		return nil, apierrors.NewConflict(s.store.resource, job.Name, fmt.Errorf("Job snapshot has not been frozen"))
	}
	digest, err := JobArchiveDigest(job)
	if err != nil {
		return nil, apierrors.NewInternalError(err)
	}
	next := jobArchiveSnapshot(job)
	next.Finalizers = []string{ebsv1.JobArtifactsCleanupFinalizer}
	doc, id, err := s.store.documentFor(ctx, next)
	if err != nil {
		return nil, err
	}
	doc.ArchivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	doc.Archive = &es.JobArchiveRecord{UID: string(job.UID), Digest: digest, Action: action}
	_, err = s.store.client.Create(ctx, "oldjob", id, doc)
	if err != nil && !es.IsStatus(err, 409) {
		return nil, s.store.apiError(err, job.Name)
	}
	// A real-time GET confirms an already-existing create without replaying an update.
	hit, err := s.store.client.Get(ctx, "oldjob", id)
	if err != nil {
		return nil, s.store.apiError(err, job.Name)
	}
	record := hit.Document.Archive
	if record == nil || record.UID != string(job.UID) || record.Digest != digest || record.Deleted {
		return nil, apierrors.NewConflict(s.store.resource, job.Name, fmt.Errorf("archive identity or frozen snapshot conflicts"))
	}
	obj, err := s.store.objectFromHit(hit)
	if err != nil {
		return nil, err
	}
	return obj.(*ebsv1.Job), nil
}

// ArchiveState returns the confirmation even after public OldJob data has been removed.
func (s *OldJobs) ArchiveState(ctx context.Context, name string) (*ebsv1.JobArchiveState, error) {
	hit, err := s.store.client.Get(ctx, "oldjob", s.store.documentID(ctx, name))
	if err != nil {
		return nil, s.store.apiError(err, name)
	}
	record := hit.Document.Archive
	if record == nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("archive confirmation is missing"))
	}
	return &ebsv1.JobArchiveState{UID: record.UID, Action: record.Action, Digest: record.Digest, Phase: "Stored"}, nil
}

package esstore

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
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

// OldJobs provides historical reads and conditional cleanup; Archive is used by the Job subresource.
type OldJobs struct {
	store *Store
}

func NewOldJobs(client *es.Client, template *genericregistry.Store) *OldJobs {
	s := New(client, "oldjob", "Job", template)
	s.resource = ebsv1.Resource("oldjobs")
	s.singular = ebsv1.Resource("oldjob")
	s.preserveSourceVersion = true
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

// Delete removes one archived snapshot, honoring supplied preconditions and dry-run.
func (s *OldJobs) Delete(ctx context.Context, name string, admission rest.ValidateObjectFunc,
	options *metav1.DeleteOptions) (runtime.Object, bool, error) {
	if options == nil {
		options = &metav1.DeleteOptions{}
	}
	namespace := request.NamespaceValue(ctx)
	if namespace == "" || name == "" {
		return nil, false, apierrors.NewBadRequest("oldjobs deletion requires a project and name")
	}
	hit, err := s.store.client.Get(ctx, "oldjob", s.store.documentID(ctx, name))
	if err != nil {
		return nil, false, s.store.apiError(err, name)
	}
	obj, err := s.store.objectFromHit(hit)
	if err != nil {
		return nil, false, err
	}
	job := obj.(*ebsv1.Job)
	if pre := options.Preconditions; pre != nil {
		if pre.UID != nil && job.UID != *pre.UID ||
			pre.ResourceVersion != nil && job.ResourceVersion != *pre.ResourceVersion {
			return nil, false, apierrors.NewConflict(s.store.resource, name, fmt.Errorf("archived Job preconditions failed"))
		}
	}
	if admission != nil {
		if err := admission(ctx, job.DeepCopy()); err != nil {
			return nil, false, err
		}
	}
	if !dryrun.IsDryRun(options.DryRun) {
		// resourceVersion belongs to the source Job; use the observed ES version for deletion.
		if err := s.store.client.Delete(ctx, "oldjob", hit.ID, hit.SeqNo, hit.PrimaryTerm); err != nil {
			return nil, false, s.store.apiError(err, name)
		}
	}
	return job, true, nil
}

// Archive uses source revisions for ordering and ES CAS for concurrent writes.
func (s *OldJobs) Archive(ctx context.Context, job *ebsv1.Job) (*ebsv1.Job, error) {
	next := job.DeepCopy()
	next.TypeMeta = metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"}
	revision, err := strconv.ParseUint(next.ResourceVersion, 10, 64)
	if err != nil || revision == 0 {
		return nil, apierrors.NewInternalError(fmt.Errorf("invalid source Job revision %q", next.ResourceVersion))
	}
	doc, id, err := s.store.documentFor(ctx, next)
	if err != nil {
		return nil, err
	}
	doc.ArchivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	hit, err := s.store.client.Get(ctx, "oldjob", id)
	if es.IsStatus(err, 404) {
		_, err = s.store.client.Create(ctx, "oldjob", id, doc)
	} else if err == nil {
		obj, decodeErr := s.store.objectFromHit(hit)
		if decodeErr != nil {
			return nil, decodeErr
		}
		old := obj.(*ebsv1.Job)
		oldRevision, parseErr := strconv.ParseUint(old.ResourceVersion, 10, 64)
		if parseErr != nil || oldRevision == 0 {
			return nil, apierrors.NewInternalError(fmt.Errorf("invalid archived Job revision %q", old.ResourceVersion))
		}
		if old.UID != next.UID || oldRevision > revision ||
			(oldRevision == revision && !equality.Semantic.DeepEqual(old, next)) {
			return nil, apierrors.NewConflict(s.store.resource, next.Name, fmt.Errorf("archived Job identity or version conflicts"))
		}
		if oldRevision == revision {
			return old, nil
		}
		_, err = s.store.client.Update(ctx, "oldjob", id, doc, hit.SeqNo, hit.PrimaryTerm)
	}
	if err != nil {
		return nil, s.store.apiError(err, next.Name)
	}
	return next, nil
}

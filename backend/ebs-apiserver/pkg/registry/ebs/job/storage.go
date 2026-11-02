package job

import (
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "jobs", Singular: "job", Kind: "Job",
		New:      func() runtime.Object { return &ebsv1.Job{} },
		NewList:  func() runtime.Object { return &ebsv1.JobList{} },
		AttrFunc: jobAttrs,
		PrepareCreate: func(obj runtime.Object) {
			j := obj.(*ebsv1.Job)
			ebsv1.SetDefaults_Job(j)
			j.Status = ebsv1.JobStatus{Phase: ebsv1.JobPending, Stage: ebsv1.JobStagePending}
			if !slices.Contains(j.Finalizers, ebsv1.JobArchiveFinalizer) {
				j.Finalizers = append(j.Finalizers, ebsv1.JobArchiveFinalizer)
			}
			delete(j.Annotations, ebsv1.JobArchiveActionAnnotation)
		},
		CopyStatus: func(obj, old runtime.Object) {
			obj.(*ebsv1.Job).Status = old.(*ebsv1.Job).Status
		},
		CopySpec: func(obj, old runtime.Object) {
			newJ, oldJ := obj.(*ebsv1.Job), old.(*ebsv1.Job)
			newJ.Spec = oldJ.Spec
			metav1.ResetObjectMetaForStatus(&newJ.ObjectMeta, &oldJ.ObjectMeta)
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateJob(obj.(*ebsv1.Job))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateJobUpdate(obj.(*ebsv1.Job), old.(*ebsv1.Job))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateJobStatusUpdate(obj.(*ebsv1.Job), old.(*ebsv1.Job))
		},
	})
}

func jobAttrs(obj runtime.Object) (labels.Set, fields.Set, error) {
	job, ok := obj.(*ebsv1.Job)
	if !ok {
		return nil, nil, fmt.Errorf("expected Job, got %T", obj)
	}
	return labels.Set(job.Labels), fields.Set{
		"metadata.name":              job.Name,
		"metadata.namespace":         job.Namespace,
		"metadata.deletionTimestamp": deletionTimestamp(job),
		"status.runner":              job.Status.Runner,
		"status.phase":               string(job.Status.Phase),
		"status.stage":               string(job.Status.Stage),
	}, nil
}

func deletionTimestamp(job *ebsv1.Job) string {
	if job.DeletionTimestamp == nil {
		return "null"
	}
	return job.DeletionTimestamp.UTC().Format("2006-01-02T15:04:05Z")
}

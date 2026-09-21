package rpmrepo

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "rpmrepos", Singular: "rpmrepo", Kind: "RpmRepo",
		New:     func() runtime.Object { return &ebsv1.RpmRepo{} },
		NewList: func() runtime.Object { return &ebsv1.RpmRepoList{} },
		PrepareCreate: func(obj runtime.Object) {
			// The Build controller seeds the inherited process repository version when it creates the
			// RpmRepo of a round. Only the baseline pair survives creation.
			repo := obj.(*ebsv1.RpmRepo)
			seeded := repo.Status.Repository
			repo.Status = ebsv1.RpmRepoStatus{}
			if seeded != nil {
				repo.Status.Repository = &ebsv1.RpmRepoRepositoryStatus{
					RepositoryUID: seeded.RepositoryUID,
					ContentURL:    seeded.ContentURL,
				}
			}
		},
		CopyStatus: func(obj, old runtime.Object) {
			obj.(*ebsv1.RpmRepo).Status = old.(*ebsv1.RpmRepo).Status
		},
		CopySpec: func(obj, old runtime.Object) {
			newRepo, oldRepo := obj.(*ebsv1.RpmRepo), old.(*ebsv1.RpmRepo)
			newRepo.Spec = oldRepo.Spec
			metav1.ResetObjectMetaForStatus(&newRepo.ObjectMeta, &oldRepo.ObjectMeta)
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			repo := obj.(*ebsv1.RpmRepo)
			allErrs := validation.ValidateRpmRepo(repo)
			// The Build controller stamps the target labels at creation so RpmRepo objects can be
			// filtered by build target without reading the same-name Build.
			labelsPath := field.NewPath("metadata", "labels")
			if repo.Labels[ebsv1.BuildTargetOSLabel] == "" {
				allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTargetOSLabel), "target OS label is required"))
			}
			if repo.Labels[ebsv1.BuildTargetArchLabel] == "" {
				allErrs = append(allErrs, field.Required(labelsPath.Key(ebsv1.BuildTargetArchLabel), "target architecture label is required"))
			}
			// A seeded baseline is only usable as a pair; reject half of it instead of persisting a
			// version UID that cannot be resolved to a content URL.
			if repository := repo.Status.Repository; repository != nil &&
				(repository.RepositoryUID == "") != (repository.ContentURL == "") {
				allErrs = append(allErrs, field.Invalid(field.NewPath("status", "repository"), repository,
					"repositoryUID and contentURL must be provided together"))
			}
			return allErrs
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateRpmRepoUpdate(obj.(*ebsv1.RpmRepo), old.(*ebsv1.RpmRepo))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateRpmRepoStatusUpdate(obj.(*ebsv1.RpmRepo), old.(*ebsv1.RpmRepo))
		},
	})
}

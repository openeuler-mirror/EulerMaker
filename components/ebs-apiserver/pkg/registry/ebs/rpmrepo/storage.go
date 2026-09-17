package rpmrepo

import (
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
			// RpmRepo of a round. Only that pair survives creation: release, conditions and every other
			// repository field stay server-owned and the repository phase always starts at Pending.
			repo := obj.(*ebsv1.RpmRepo)
			seeded := repo.Status.Repository
			repo.Status = ebsv1.RpmRepoStatus{
				Repository: &ebsv1.RpmRepoRepositoryStatus{Phase: ebsv1.RpmRepoPending},
			}
			if seeded != nil {
				repo.Status.Repository.RepositoryUID = seeded.RepositoryUID
				repo.Status.Repository.ContentURL = seeded.ContentURL
			}
		},
		CopyStatus: func(obj, old runtime.Object) {
			obj.(*ebsv1.RpmRepo).Status = old.(*ebsv1.RpmRepo).Status
		},
		CopySpec: func(obj, old runtime.Object) {
			obj.(*ebsv1.RpmRepo).Spec = old.(*ebsv1.RpmRepo).Spec
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			repo := obj.(*ebsv1.RpmRepo)
			allErrs := validation.ValidateRpmRepo(repo)
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

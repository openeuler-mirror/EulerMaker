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
			// The Build controller seeds the inherited process repository version and the initial
			// process repository phase when it creates the RpmRepo of a round: Ready together with a
			// complete baseline, Processing otherwise. Only that phase and the baseline pair survive
			// creation; release, conditions and every other repository field stay server-owned.
			// A missing phase defaults to Processing; anything else is passed through so the create
			// validation below can reject it.
			repo := obj.(*ebsv1.RpmRepo)
			seeded := repo.Status.Repository
			repo.Status = ebsv1.RpmRepoStatus{
				Repository: &ebsv1.RpmRepoRepositoryStatus{Phase: ebsv1.RpmRepoProcessing},
			}
			if seeded != nil {
				if seeded.Phase != "" {
					repo.Status.Repository.Phase = seeded.Phase
				}
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
			// The create request may only pick the initial process repository phase: Ready when it
			// inherits a complete baseline, Processing otherwise. Later phases belong to the RpmRepo
			// controller, so other values (including a Ready without a baseline) are rejected here and
			// not in ValidateRpmRepo, which also guards ordinary updates.
			if repository := repo.Status.Repository; repository != nil {
				switch repository.Phase {
				case ebsv1.RpmRepoProcessing:
				case ebsv1.RpmRepoReady:
					if repository.RepositoryUID == "" || repository.ContentURL == "" {
						allErrs = append(allErrs, field.Invalid(field.NewPath("status", "repository", "phase"), repository.Phase,
							"phase Ready requires a complete repository baseline"))
					}
				default:
					allErrs = append(allErrs, field.NotSupported(field.NewPath("status", "repository", "phase"), repository.Phase,
						[]string{string(ebsv1.RpmRepoProcessing), string(ebsv1.RpmRepoReady)}))
				}
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

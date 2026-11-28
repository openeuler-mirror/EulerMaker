package project

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	internalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/storage/es"
)

// ValidateQuota checks the current owner count; concurrent creates may exceed the limit.
func ValidateQuota(client *es.Client, countProjects func(context.Context, *internalversion.ListOptions) (int64, error)) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		project := obj.(*ebsv1.Project)
		owner := project.Labels[ebsv1.ProjectOwnerUserLabel]
		ownerPath := field.NewPath("metadata", "labels").Key(ebsv1.ProjectOwnerUserLabel)
		if messages := utilvalidation.IsDNS1123Label(owner); len(messages) != 0 {
			return apierrors.NewInvalid(ebsv1.Kind("Project"), project.Name, field.ErrorList{
				field.Invalid(ownerPath, owner, "an owner User name is required"),
			})
		}
		hit, err := client.GetWithSourceFilter(ctx, "user", owner, es.SourceFilter{
			Includes: []string{"data.metadata.name", "data.metadata.uid", "data.metadata.deletionTimestamp", "data.spec.quota"},
		})
		if es.IsDocumentNotFound(err) {
			return apierrors.NewInvalid(ebsv1.Kind("Project"), project.Name, field.ErrorList{
				field.Invalid(ownerPath, owner, "owner User does not exist"),
			})
		}
		if err != nil {
			return apierrors.NewServiceUnavailable("project quota User is unavailable")
		}
		var doc es.Document
		var user iamv1.User
		if err := hit.Decode(&doc); err != nil {
			return apierrors.NewInternalError(err)
		}
		if err := json.Unmarshal(doc.Data, &user); err != nil {
			return apierrors.NewInternalError(err)
		}
		if user.UID == "" || user.Name != owner || user.DeletionTimestamp != nil {
			return apierrors.NewServiceUnavailable("project quota User is invalid or deleting")
		}
		if user.Spec.Quota == nil || user.Spec.Quota.Projects == nil {
			return nil
		}
		used, err := countProjects(ctx, &internalversion.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{ebsv1.ProjectOwnerUserLabel: owner}),
		})
		if err != nil {
			return apierrors.NewServiceUnavailable("project quota count is unavailable")
		}
		if used >= int64(*user.Spec.Quota.Projects) {
			return apierrors.NewForbidden(ebsv1.Resource("projects"), project.Name, fmt.Errorf(
				"ProjectQuotaExceeded: owner=%s limit=%d used=%d", owner, *user.Spec.Quota.Projects, used))
		}
		return nil
	}
}

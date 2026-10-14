package build

import (
	"context"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

// CreateStorage exposes normal Build storage and owns its coordination service.
type CreateStorage struct {
	*esstore.Store
	claims *claimManager
}

func NewCreateStorage(store *esstore.Store, backend coordinationBackend) *CreateStorage {
	manager := newClaimManager(backend, store, store)
	store.SetCreateTransaction(manager.create)
	store.SetAfterWrite(manager.afterWrite)
	return &CreateStorage{Store: store, claims: manager}
}

func (s *CreateStorage) RunRecovery(ctx context.Context) { s.claims.run(ctx) }

func limitedBuild(build *ebsv1.Build) bool {
	switch build.Spec.BuildType {
	case "", "full", "incremental", "specified":
		return true
	default:
		return false
	}
}

var _ coordinationBackend = (*es.Client)(nil)

func validateLatestBuild(ctx context.Context, builds rest.Lister, build *ebsv1.Build) error {
	selector := labels.SelectorFromSet(labels.Set{
		ebsv1.BuildTargetOSLabel:   build.Spec.BuildTarget.Os,
		ebsv1.BuildTargetArchLabel: build.Spec.BuildTarget.Arch,
	})
	nonSingle, err := labels.NewRequirement(ebsv1.BuildTypeLabel, selection.NotEquals, []string{"single"})
	if err != nil {
		return err
	}
	// ES lists newest creationTimestamp first. Do not filter phase: only the
	// latest non-single Build is the gate, not all historical active Builds.
	obj, err := builds.List(ctx, &internalversion.ListOptions{LabelSelector: selector.Add(*nonSingle), Limit: 1})
	if err != nil {
		return err
	}
	list, ok := obj.(*ebsv1.BuildList)
	if !ok {
		return apierrors.NewInternalError(fmt.Errorf("expected BuildList, got %T", obj))
	}
	if len(list.Items) == 0 || list.Items[0].Status.Phase.IsTerminal() {
		return nil
	}
	previous := &list.Items[0]
	return apierrors.NewConflict(ebsv1.Resource("builds"), build.Name,
		fmt.Errorf("latest non-single build %q for target %s/%s is not terminal (phase %q)", previous.Name, build.Spec.BuildTarget.Os, build.Spec.BuildTarget.Arch, previous.Status.Phase))
}

func validateFullBuildBaseline(ctx context.Context, builds rest.Lister, build *ebsv1.Build) error {
	if build.Spec.BuildType != "incremental" && build.Spec.BuildType != "specified" {
		return nil
	}
	obj, err := builds.List(ctx, &internalversion.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			ebsv1.BuildTargetOSLabel:   build.Spec.BuildTarget.Os,
			ebsv1.BuildTargetArchLabel: build.Spec.BuildTarget.Arch,
			ebsv1.BuildTypeLabel:       "full",
		}),
		FieldSelector: fields.OneTermEqualSelector("status.phase", string(ebsv1.BuildSuccess)),
		Limit:         1,
	})
	if err != nil {
		return err
	}
	list, ok := obj.(*ebsv1.BuildList)
	if !ok {
		return apierrors.NewInternalError(fmt.Errorf("expected BuildList, got %T", obj))
	}
	if len(list.Items) != 0 {
		return nil
	}
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Reason:   metav1.StatusReason("FullBuildRequired"),
		Message:  "No complete full build exists yet for this project and target",
		Code:     http.StatusPreconditionFailed,
	}}
}

package buildgraph

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "buildgraphs", Singular: "buildgraph", Kind: "BuildGraph",
		New:        func() runtime.Object { return &ebsv1.BuildGraph{} },
		NewList:    func() runtime.Object { return &ebsv1.BuildGraphList{} },
		CopyStatus: func(obj, old runtime.Object) { obj.(*ebsv1.BuildGraph).Status = old.(*ebsv1.BuildGraph).Status },
		CopySpec: func(obj, old runtime.Object) {
			graph, prev := obj.(*ebsv1.BuildGraph), old.(*ebsv1.BuildGraph)
			rv := graph.ResourceVersion
			graph.ObjectMeta = *prev.ObjectMeta.DeepCopy()
			graph.ResourceVersion = rv
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateBuildGraph(obj.(*ebsv1.BuildGraph))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildGraphUpdate(obj.(*ebsv1.BuildGraph), old.(*ebsv1.BuildGraph))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateBuildGraphStatusUpdate(obj.(*ebsv1.BuildGraph), old.(*ebsv1.BuildGraph))
		},
	})
}

// ValidateOwner verifies the referenced BuildInfo before creating a graph.
func ValidateOwner(buildinfos rest.Getter) rest.ValidateObjectFunc {
	return func(ctx context.Context, obj runtime.Object) error {
		graph := obj.(*ebsv1.BuildGraph)
		value, err := buildinfos.Get(ctx, graph.Name, &metav1.GetOptions{})
		if err != nil {
			return err
		}
		info, ok := value.(*ebsv1.BuildInfo)
		owner := metav1.GetControllerOf(graph)
		if !ok || info.Namespace != graph.Namespace || owner == nil || owner.UID != info.UID || info.DeletionTimestamp != nil {
			return apierrors.NewInvalid(ebsv1.Kind("BuildGraph"), graph.Name,
				field.ErrorList{field.Invalid(field.NewPath("metadata", "ownerReferences"), graph.OwnerReferences, "BuildInfo owner is unavailable or has changed")})
		}
		return nil
	}
}

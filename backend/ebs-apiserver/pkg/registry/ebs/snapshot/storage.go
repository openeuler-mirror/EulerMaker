package snapshot

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
	"ebs-apiserver/pkg/registry/ebs/scopedresource"
)

func NewStorage() *scopedresource.Storage {
	return scopedresource.NewStorage(scopedresource.Config{
		Resource: "snapshots", Singular: "snapshot", Kind: "Snapshot",
		New:     func() runtime.Object { return &ebsv1.Snapshot{} },
		NewList: func() runtime.Object { return &ebsv1.SnapshotList{} },
		PrepareCreate: func(obj runtime.Object) {
			snap := obj.(*ebsv1.Snapshot)
			snap.Status = ebsv1.SnapshotStatus{Phase: ebsv1.SnapshotPending}
		},
		CopyStatus: func(obj, old runtime.Object) {
			newSnap := obj.(*ebsv1.Snapshot)
			oldSnap := old.(*ebsv1.Snapshot)
			newSnap.Status = oldSnap.Status
		},
		CopySpec: func(obj, old runtime.Object) {
			newSnap := obj.(*ebsv1.Snapshot)
			oldSnap := old.(*ebsv1.Snapshot)
			newSnap.Spec = oldSnap.Spec
		},
		Validate: func(obj runtime.Object) field.ErrorList {
			return validation.ValidateSnapshot(obj.(*ebsv1.Snapshot))
		},
		ValidateUpdate: func(obj, old runtime.Object) field.ErrorList {
			return validation.ValidateSnapshotUpdate(obj.(*ebsv1.Snapshot), old.(*ebsv1.Snapshot))
		},
		ValidateStatus: func(obj, old runtime.Object) field.ErrorList {
			return field.ErrorList{}
		},
	})
}

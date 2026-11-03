package buildinfo

import (
	"context"
	"slices"
	"testing"

	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"

	ebsv1 "ebs-api/ebs/v1"
)

func TestCreateDefaultsCleanupFinalizer(t *testing.T) {
	store := NewStorage().Resource.(*genericregistry.Store)
	info := &ebsv1.BuildInfo{}
	info.Finalizers = []string{"other/controller"}
	store.CreateStrategy.PrepareForCreate(context.Background(), info)
	store.CreateStrategy.PrepareForCreate(context.Background(), info)
	if !slices.Equal(info.Finalizers, []string{"other/controller", ebsv1.BuildInfoCleanupFinalizer}) ||
		info.Status.Phase != ebsv1.BuildInfoPending {
		t.Fatalf("defaults=%+v", info)
	}
}

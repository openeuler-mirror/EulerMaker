package snapshot

import (
	"context"
	"fmt"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

type Client interface {
	GetSnapshot(context.Context, string, string) (*ebsv1.Snapshot, error)
	GetBuild(context.Context, string, string) (*ebsv1.Build, error)
	UpdateSnapshotStatus(context.Context, *ebsv1.Snapshot) (*ebsv1.Snapshot, error)
}

type apiClient struct{ client clientpkg.Interface }

func newAPIClient(value clientpkg.Interface) Client { return &apiClient{client: value} }

func (c *apiClient) GetSnapshot(ctx context.Context, namespace, name string) (*ebsv1.Snapshot, error) {
	obj, err := c.client.Get(ctx, source.SnapshotsGVR, namespace, name)
	if err != nil {
		return nil, err
	}
	snapshot, ok := obj.(*ebsv1.Snapshot)
	if !ok || snapshot == nil || snapshot.Name != name || snapshot.Namespace != namespace || snapshot.UID == "" || snapshot.ResourceVersion == "" {
		return nil, fmt.Errorf("unexpected Snapshot response for %s/%s: %T", namespace, name, obj)
	}
	return snapshot, nil
}

func (c *apiClient) GetBuild(ctx context.Context, namespace, name string) (*ebsv1.Build, error) {
	obj, err := c.client.Get(ctx, source.BuildsGVR, namespace, name)
	if err != nil {
		return nil, err
	}
	build, ok := obj.(*ebsv1.Build)
	if !ok || build == nil || build.Name != name || build.Namespace != namespace || build.UID == "" || build.ResourceVersion == "" {
		return nil, fmt.Errorf("unexpected Build response for %s/%s: %T", namespace, name, obj)
	}
	return build, nil
}

func (c *apiClient) UpdateSnapshotStatus(ctx context.Context, request *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
	if request == nil {
		return nil, snapshotWriteUnknown(fmt.Errorf("nil Snapshot request"))
	}
	obj, err := c.client.UpdateStatus(ctx, source.SnapshotsGVR, request.Namespace, request)
	if err != nil {
		return nil, err
	}
	updated, ok := obj.(*ebsv1.Snapshot)
	if !ok || updated == nil || updated.UID != request.UID || updated.Name != request.Name || updated.Namespace != request.Namespace || updated.ResourceVersion == "" {
		return nil, snapshotWriteUnknown(fmt.Errorf("unexpected Snapshot status response: %T", obj))
	}
	return updated, nil
}

func snapshotWriteUnknown(err error) error {
	return &clientpkg.WriteError{Operation: "update-status", Resource: source.SnapshotsGVR.GroupResource(), Outcome: clientpkg.WriteUnknown, Err: err}
}

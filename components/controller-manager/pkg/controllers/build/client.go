package build

import (
	"context"
	"fmt"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// lastPublishedBuildFieldSelector narrows the search to Builds that finished the publish stage successfully.
const lastPublishedBuildFieldSelector = "status.phase=Success,status.stage=publish"

// Client is the typed API surface the Build controller depends on.
// It is implemented by wrapping the shared controller-manager API client, so write errors keep the
// shared NotSent / Rejected / Unknown classification and are never re-derived from status codes.
type Client interface {
	GetProject(ctx context.Context, project string) (*ebsv1.Project, error)
	GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error)
	GetLastPublishedBuild(ctx context.Context, project, targetOS, targetArch string) (*ebsv1.Build, error)
	GetSnapshot(ctx context.Context, project, name string) (*ebsv1.Snapshot, error)
	GetRpmRepo(ctx context.Context, project, name string) (*ebsv1.RpmRepo, error)
	GetBuildInfo(ctx context.Context, project, name string) (*ebsv1.BuildInfo, error)

	CreateSnapshot(ctx context.Context, project string, obj *ebsv1.Snapshot) (*ebsv1.Snapshot, error)
	CreateRpmRepo(ctx context.Context, project string, obj *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error)
	CreateBuildInfo(ctx context.Context, project string, obj *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error)

	UpdateBuildStatus(ctx context.Context, obj *ebsv1.Build) (*ebsv1.Build, error)
}

type apiClient struct{ client clientpkg.Interface }

func newAPIClient(value clientpkg.Interface) Client { return &apiClient{client: value} }

// contractError reports a request or response contract violation: invalid client input, or a server response
// that does not match the requested resource identity. Retrying cannot fix it, so read paths classify it as a
// permanent error instead of entering the retry backoff.
type contractError struct{ err error }

func (e contractError) Error() string { return e.err.Error() }

func (e contractError) Unwrap() error { return e.err }

func contractErrorf(format string, args ...any) error {
	return contractError{err: fmt.Errorf(format, args...)}
}

func (c *apiClient) GetProject(ctx context.Context, project string) (*ebsv1.Project, error) {
	if project == "" {
		return nil, contractErrorf("project is required")
	}
	obj, err := c.client.Get(ctx, source.ProjectsGVR, "", project)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Project)
	if !ok || value == nil || value.Name != project || value.Namespace != "" || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Project response for %s: %T", project, obj)
	}
	return value, nil
}

func (c *apiClient) GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error) {
	obj, err := c.client.Get(ctx, source.BuildsGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Build)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Build response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

// GetLastPublishedBuild returns the most recent Build of the project that published successfully for the
// given target OS and architecture, or nil when the project has no such Build yet.
// The lookup stays inside one Project: a cluster-wide limit=1 query would select another Project's Build.
func (c *apiClient) GetLastPublishedBuild(ctx context.Context, project, targetOS, targetArch string) (*ebsv1.Build, error) {
	selector := labels.Set{
		ebsv1.BuildTargetOSLabel:   targetOS,
		ebsv1.BuildTargetArchLabel: targetArch,
	}.String()
	page, err := c.client.ListProjectPage(ctx, source.BuildsGVR, project, metav1.ListOptions{
		LabelSelector: selector,
		FieldSelector: lastPublishedBuildFieldSelector,
		Limit:         1,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Items) == 0 {
		return nil, nil
	}
	value, ok := page.Items[0].(*ebsv1.Build)
	if !ok || value == nil || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected published Build response for %s: %T", project, page.Items[0])
	}
	return value, nil
}

func (c *apiClient) GetSnapshot(ctx context.Context, project, name string) (*ebsv1.Snapshot, error) {
	obj, err := c.client.Get(ctx, source.SnapshotsGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Snapshot)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Snapshot response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetRpmRepo(ctx context.Context, project, name string) (*ebsv1.RpmRepo, error) {
	obj, err := c.client.Get(ctx, source.RpmReposGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.RpmRepo)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected RpmRepo response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetBuildInfo(ctx context.Context, project, name string) (*ebsv1.BuildInfo, error) {
	obj, err := c.client.Get(ctx, source.BuildInfosGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.BuildInfo)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected BuildInfo response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) CreateSnapshot(ctx context.Context, project string, obj *ebsv1.Snapshot) (*ebsv1.Snapshot, error) {
	if obj == nil {
		return nil, notSentWrite("create", source.SnapshotsGVR, fmt.Errorf("Snapshot request is required"))
	}
	created, err := c.client.Create(ctx, source.SnapshotsGVR, project, obj)
	if err != nil {
		return nil, err
	}
	value, ok := created.(*ebsv1.Snapshot)
	if !ok || value == nil || value.Name != obj.Name || value.Namespace != obj.Namespace || value.UID == "" || value.ResourceVersion == "" {
		return nil, unknownWrite("create", source.SnapshotsGVR, fmt.Errorf("unexpected Snapshot create response: %T", created))
	}
	return value, nil
}

func (c *apiClient) CreateRpmRepo(ctx context.Context, project string, obj *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
	if obj == nil {
		return nil, notSentWrite("create", source.RpmReposGVR, fmt.Errorf("RpmRepo request is required"))
	}
	created, err := c.client.Create(ctx, source.RpmReposGVR, project, obj)
	if err != nil {
		return nil, err
	}
	value, ok := created.(*ebsv1.RpmRepo)
	if !ok || value == nil || value.Name != obj.Name || value.Namespace != obj.Namespace || value.UID == "" || value.ResourceVersion == "" {
		return nil, unknownWrite("create", source.RpmReposGVR, fmt.Errorf("unexpected RpmRepo create response: %T", created))
	}
	return value, nil
}

func (c *apiClient) CreateBuildInfo(ctx context.Context, project string, obj *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
	if obj == nil {
		return nil, notSentWrite("create", source.BuildInfosGVR, fmt.Errorf("BuildInfo request is required"))
	}
	created, err := c.client.Create(ctx, source.BuildInfosGVR, project, obj)
	if err != nil {
		return nil, err
	}
	value, ok := created.(*ebsv1.BuildInfo)
	if !ok || value == nil || value.Name != obj.Name || value.Namespace != obj.Namespace || value.UID == "" || value.ResourceVersion == "" {
		return nil, unknownWrite("create", source.BuildInfosGVR, fmt.Errorf("unexpected BuildInfo create response: %T", created))
	}
	return value, nil
}

func (c *apiClient) UpdateBuildStatus(ctx context.Context, obj *ebsv1.Build) (*ebsv1.Build, error) {
	if obj == nil {
		return nil, notSentWrite("update-status", source.BuildsGVR, fmt.Errorf("Build request is required"))
	}
	updated, err := c.client.UpdateStatus(ctx, source.BuildsGVR, obj.Namespace, obj)
	if err != nil {
		return nil, err
	}
	value, ok := updated.(*ebsv1.Build)
	if !ok || value == nil || value.UID != obj.UID || value.Name != obj.Name || value.Namespace != obj.Namespace || value.ResourceVersion == "" {
		return nil, unknownWrite("update-status", source.BuildsGVR, fmt.Errorf("unexpected Build status response: %T", updated))
	}
	return value, nil
}

func notSentWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: clientpkg.WriteNotSent, Err: err}
}

func unknownWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: clientpkg.WriteUnknown, Err: err}
}

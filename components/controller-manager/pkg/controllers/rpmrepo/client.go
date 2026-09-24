package rpmrepo

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// defaultListPageSize is used when a caller leaves ListOptions.Limit unset.
const defaultListPageSize = 500

// maxListPages bounds one complete list read: a server that keeps handing out fresh cursors must not be able to
// keep the controller paging forever.
const maxListPages = 1000

// Client is the typed API surface this controller depends on. It is implemented on top of the shared
// controller-manager client so write errors keep the NotSent / Rejected / Unknown classification.
type Client interface {
	GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error)
	GetBuildInfo(ctx context.Context, project, name string) (*ebsv1.BuildInfo, error)
	GetRpmRepo(ctx context.Context, project, name string) (*ebsv1.RpmRepo, error)
	ListJobs(ctx context.Context, project string, options metav1.ListOptions) ([]ebsv1.Job, error)
	ListRpmRepos(ctx context.Context, project string, options metav1.ListOptions) ([]ebsv1.RpmRepo, error)
	UpdateRpmRepoStatus(ctx context.Context, obj *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error)
}

type apiClient struct{ client clientpkg.Interface }

func newAPIClient(value clientpkg.Interface) Client { return &apiClient{client: value} }

// contractError marks a request or response that violates the client contract. Retrying cannot fix it.
type contractError struct{ err error }

func (e contractError) Error() string { return e.err.Error() }

func (e contractError) Unwrap() error { return e.err }

func contractErrorf(format string, args ...any) error {
	return contractError{err: fmt.Errorf(format, args...)}
}

func (c *apiClient) GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error) {
	obj, err := c.client.Get(ctx, source.BuildsGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Build)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" {
		return nil, contractErrorf("unexpected Build response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetBuildInfo(ctx context.Context, project, name string) (*ebsv1.BuildInfo, error) {
	obj, err := c.client.Get(ctx, source.BuildInfosGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.BuildInfo)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" {
		return nil, contractErrorf("unexpected BuildInfo response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetRpmRepo(ctx context.Context, project, name string) (*ebsv1.RpmRepo, error) {
	obj, err := c.client.Get(ctx, source.RpmReposGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.RpmRepo)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" {
		return nil, contractErrorf("unexpected RpmRepo response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) ListJobs(ctx context.Context, project string, options metav1.ListOptions) ([]ebsv1.Job, error) {
	items, err := c.listAll(ctx, source.JobsGVR, project, options)
	if err != nil {
		return nil, err
	}
	result := make([]ebsv1.Job, 0, len(items))
	for _, item := range items {
		job, ok := item.(*ebsv1.Job)
		if !ok || job == nil || job.UID == "" {
			return nil, contractErrorf("unexpected Job list item: %T", item)
		}
		result = append(result, *job)
	}
	return result, nil
}

func (c *apiClient) ListRpmRepos(ctx context.Context, project string, options metav1.ListOptions) ([]ebsv1.RpmRepo, error) {
	items, err := c.listAll(ctx, source.RpmReposGVR, project, options)
	if err != nil {
		return nil, err
	}
	result := make([]ebsv1.RpmRepo, 0, len(items))
	for _, item := range items {
		repo, ok := item.(*ebsv1.RpmRepo)
		if !ok || repo == nil || repo.UID == "" {
			return nil, contractErrorf("unexpected RpmRepo list item: %T", item)
		}
		result = append(result, *repo)
	}
	return result, nil
}

// listAll implements the complete paging contract: every call starts from the first page, passes the returned
// cursor through unchanged and only finishes when the server stops returning one. Partial results are
// discarded on failure so callers never treat a truncated list as a complete one.
func (c *apiClient) listAll(ctx context.Context, gvr schema.GroupVersionResource, project string, options metav1.ListOptions) ([]runtime.Object, error) {
	request := options
	if request.Limit < 0 {
		return nil, contractErrorf("negative list limit is not supported for %s", gvr.Resource)
	}
	if request.Limit == 0 {
		request.Limit = defaultListPageSize
	}
	request.Continue = ""
	seen := make(map[string]struct{})
	// Limit is a page size, not a result bound: sizing the accumulator by it would let a large caller value
	// preallocate the whole list before a single page arrived.
	capacity := request.Limit
	if capacity > defaultListPageSize {
		capacity = defaultListPageSize
	}
	items := make([]runtime.Object, 0, capacity)
	pages := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pages++
		if pages > maxListPages {
			return nil, contractErrorf("list %s exceeded %d pages", gvr.Resource, maxListPages)
		}
		page, err := c.client.ListProjectPage(ctx, gvr, project, request)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Continue == "" {
			return items, nil
		}
		if _, exists := seen[page.Continue]; exists {
			return nil, contractErrorf("list %s repeated continue cursor", gvr.Resource)
		}
		seen[page.Continue] = struct{}{}
		request.Continue = page.Continue
	}
}

func (c *apiClient) UpdateRpmRepoStatus(ctx context.Context, obj *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
	if obj == nil {
		return nil, notSentWrite("update-status", source.RpmReposGVR, fmt.Errorf("RpmRepo request is required"))
	}
	updated, err := c.client.UpdateStatus(ctx, source.RpmReposGVR, obj.Namespace, obj)
	if err != nil {
		return nil, err
	}
	value, ok := updated.(*ebsv1.RpmRepo)
	if !ok || value == nil || value.UID != obj.UID || value.Name != obj.Name || value.Namespace != obj.Namespace || value.ResourceVersion == "" {
		return nil, unknownWrite("update-status", source.RpmReposGVR, fmt.Errorf("unexpected RpmRepo status response: %T", updated))
	}
	// RpmRepoSpec is empty today, so this guard cannot fire yet: the real protection against a spec rewrite is
	// the /status subresource. It stays here so a future non-empty spec is still checked.
	if value.Spec != obj.Spec {
		return nil, unknownWrite("update-status", source.RpmReposGVR, fmt.Errorf("RpmRepo status response did not preserve spec"))
	}
	return value, nil
}

func notSentWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: clientpkg.WriteNotSent, Err: err}
}

func unknownWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: clientpkg.WriteUnknown, Err: err}
}

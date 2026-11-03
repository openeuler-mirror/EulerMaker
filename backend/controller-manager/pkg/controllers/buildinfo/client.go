package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// ErrNotFound is the sentinel returned by read methods when the requested object does not exist, so callers distinguish
// "missing" from "query failed".
var ErrNotFound = errors.New("object not found")

// apiServerReadRetries is the fixed in-process retry budget for transient apiserver query failures (network errors,
// timeouts, 5xx). It is a code constant, unlike the configurable git-server retry budget.
const apiServerReadRetries = 3

// Client is the typed API surface the BuildInfo controller depends on.
//
// BuildInfo status writes go through /status; the apiserver keeps the old spec. specDepends is an in-memory view and is
// never persisted. Cleanup uses a main-resource PUT only to remove its finalizer. Write failures are returned as
// *WriteError identifiable via errors.As; the controller never re-derives the outcome from status codes or error text.
type Client interface {
	// GetBuildInfo re-gets the latest BuildInfo at reconcile entry.
	GetBuildInfo(ctx context.Context, namespace, name string) (*ebsv1.BuildInfo, error)
	// UpdateBuildInfoStatus writes BuildInfo.status via /status with the object's resourceVersion as an optimistic lock.
	UpdateBuildInfoStatus(ctx context.Context, obj *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error)
	GetBuildGraph(ctx context.Context, project, name string, metadataOnly bool) (*ebsv1.BuildGraph, error)
	CreateBuildGraph(ctx context.Context, project string, obj *ebsv1.BuildGraph) (*ebsv1.BuildGraph, error)
	UpdateBuildGraphStatus(ctx context.Context, obj *ebsv1.BuildGraph) (*ebsv1.BuildGraph, error)

	// CreateJob creates a Job. Unknown outcomes are confirmed by GET on its deterministic name.
	CreateJob(ctx context.Context, project string, obj *ebsv1.Job) (*ebsv1.Job, error)
	GetJob(ctx context.Context, project, name string) (*ebsv1.Job, error)
	// GetOldJob reads an archived Job with the same response validation as GetJob.
	GetOldJob(ctx context.Context, project, name string) (*ebsv1.Job, error)
	AbortJob(context.Context, string, string, types.UID, string) (*ebsv1.Job, error)
	// ListJobs reads all matching Jobs; an optional field projection must retain metadata.
	ListJobs(ctx context.Context, project string, selector labels.Selector, includeFields ...string) ([]ebsv1.Job, error)
	// ListOldJobs reads all matching archived Jobs with the same pagination and projection contract.
	ListOldJobs(ctx context.Context, project string, selector labels.Selector, includeFields ...string) ([]ebsv1.Job, error)

	// GetBuild reads the parent Build without modifying it.
	GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error)
	GetRpmRepo(ctx context.Context, project, name string) (*ebsv1.RpmRepo, error)
	GetSnapshot(ctx context.Context, project, name string, includeFields ...string) (*ebsv1.Snapshot, error)
	GetProject(ctx context.Context, project string) (*ebsv1.Project, error)
	GetScript(ctx context.Context, name string) (*ebsv1.Script, error)
	// GetBuildResourceContent reads the cluster-wide default resource table.
	GetBuildResourceContent(ctx context.Context) (*ebsv1.BuildResourceContent, error)

	// FinalizeBuildInfo removes the cleanup finalizer through the main resource endpoint.
	FinalizeBuildInfo(context.Context, *ebsv1.BuildInfo) error
	// DeleteCleanupChild deletes a child with UID and resourceVersion preconditions.
	DeleteCleanupChild(context.Context, string, CleanupTarget, metav1.Object) error
}

// newAPIClient wraps the shared controller-manager API client.
func newAPIClient(client apiserver.Interface) Client { return &apiClient{client: client} }

type apiClient struct{ client apiserver.Interface }

// CleanupTarget identifies a BuildInfo-owned child within its Project.
type CleanupTarget struct{ Resource, Name string }

func (c *apiClient) DeleteCleanupChild(ctx context.Context, project string, target CleanupTarget, obj metav1.Object) error {
	return c.client.Delete(ctx, ebsv1.SchemeGroupVersion.WithResource(target.Resource), project, target.Name,
		apiserver.DeletePreconditions{UID: obj.GetUID(), ResourceVersion: obj.GetResourceVersion()})
}

func (c *apiClient) FinalizeBuildInfo(ctx context.Context, info *ebsv1.BuildInfo) error {
	obj, err := c.client.Update(ctx, ebsv1.SchemeGroupVersion.WithResource("buildinfos"), info.Namespace, info)
	if err != nil {
		return err
	}
	updated, ok := obj.(*ebsv1.BuildInfo)
	if !ok || updated.UID != info.UID || updated.Name != info.Name || updated.Namespace != info.Namespace ||
		slices.Contains(updated.Finalizers, ebsv1.BuildInfoCleanupFinalizer) {
		return fmt.Errorf("BuildInfo cleanup update was not confirmed")
	}
	return nil
}

func (c *apiClient) AbortJob(
	ctx context.Context,
	project, name string,
	uid types.UID,
	reason string,
) (*ebsv1.Job, error) {
	if project == "" || name == "" || uid == "" {
		return nil, notSentWrite("abort", source.JobsGVR, fmt.Errorf("project, Job name and UID are required"))
	}
	body := struct {
		UID    types.UID `json:"uid"`
		Reason string    `json:"reason"`
	}{UID: uid, Reason: reason}
	obj, err := c.client.PostSubresource(ctx, source.JobsGVR, project, name, "abort", body)
	if err != nil {
		return nil, err
	}
	job, ok := obj.(*ebsv1.Job)
	if !ok || job == nil || job.Namespace != project || job.Name != name || job.UID != uid ||
		job.ResourceVersion == "" || !job.Status.Phase.IsTerminal() {
		return nil, unknownWrite("abort", source.JobsGVR, fmt.Errorf("unexpected Job abort response identity or phase"))
	}
	return job, nil
}

var _ Client = (*apiClient)(nil)

// contractError reports a request or response contract violation: invalid client input, or a server response that does
// not match the requested resource identity. Retrying cannot fix it, so read paths classify it as a permanent error
// instead of entering the retry backoff.
type contractError struct{ err error }

func (e contractError) Error() string { return e.err.Error() }

func (e contractError) Unwrap() error { return e.err }

func contractErrorf(format string, args ...any) error {
	return contractError{err: fmt.Errorf(format, args...)}
}

func (c *apiClient) GetBuildInfo(ctx context.Context, namespace, name string) (*ebsv1.BuildInfo, error) {
	if namespace == "" || name == "" {
		return nil, contractErrorf("namespace and name are required")
	}
	obj, err := c.readGet(ctx, source.BuildInfosGVR, namespace, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.BuildInfo)
	if !ok || value == nil || value.Name != name || value.Namespace != namespace || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected BuildInfo response for %s/%s: %T", namespace, name, obj)
	}
	return value, nil
}

func (c *apiClient) UpdateBuildInfoStatus(ctx context.Context, obj *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error) {
	if obj == nil {
		return nil, notSentWrite("update-status", source.BuildInfosGVR, fmt.Errorf("BuildInfo request is required"))
	}
	updated, err := c.client.UpdateStatus(ctx, source.BuildInfosGVR, obj.Namespace, obj)
	if err != nil {
		return nil, err
	}
	value, ok := updated.(*ebsv1.BuildInfo)
	if !ok || value == nil || value.UID != obj.UID || value.Name != obj.Name || value.Namespace != obj.Namespace ||
		value.ResourceVersion == "" {
		return nil, unknownWrite(
			"update-status",
			source.BuildInfosGVR,
			fmt.Errorf("unexpected BuildInfo status response: %T", updated),
		)
	}
	return value, nil
}

func (c *apiClient) CreateJob(ctx context.Context, project string, obj *ebsv1.Job) (*ebsv1.Job, error) {
	if obj == nil {
		return nil, notSentWrite("create", source.JobsGVR, fmt.Errorf("Job request is required"))
	}
	created, err := c.client.Create(ctx, source.JobsGVR, project, obj)
	if err != nil {
		return nil, err
	}
	value, ok := created.(*ebsv1.Job)
	if !ok || value == nil || value.Name != obj.Name || value.Namespace != obj.Namespace || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, unknownWrite("create", source.JobsGVR, fmt.Errorf("unexpected Job create response: %T", created))
	}
	return value, nil
}

func (c *apiClient) GetBuildGraph(ctx context.Context, project, name string, metadataOnly bool) (*ebsv1.BuildGraph, error) {
	fields := ""
	if metadataOnly {
		fields = "metadata"
	}
	obj, err := c.readGet(ctx, source.BuildGraphsGVR, project, name, fields)
	if err != nil {
		return nil, err
	}
	graph, ok := obj.(*ebsv1.BuildGraph)
	if !ok || graph == nil || graph.Name != name || graph.Namespace != project || graph.UID == "" || graph.ResourceVersion == "" {
		return nil, contractErrorf("unexpected BuildGraph response for %s/%s: %T", project, name, obj)
	}
	return graph, nil
}

func (c *apiClient) CreateBuildGraph(ctx context.Context, project string, obj *ebsv1.BuildGraph) (*ebsv1.BuildGraph, error) {
	if obj == nil {
		return nil, notSentWrite("create", source.BuildGraphsGVR, fmt.Errorf("BuildGraph is required"))
	}
	result, err := c.client.Create(ctx, source.BuildGraphsGVR, project, obj)
	return buildGraphWriteResponse("create", obj, result, err)
}

func (c *apiClient) UpdateBuildGraphStatus(ctx context.Context, obj *ebsv1.BuildGraph) (*ebsv1.BuildGraph, error) {
	if obj == nil {
		return nil, notSentWrite("update-status", source.BuildGraphsGVR, fmt.Errorf("BuildGraph is required"))
	}
	result, err := c.client.UpdateStatus(ctx, source.BuildGraphsGVR, obj.Namespace, obj)
	return buildGraphWriteResponse("update-status", obj, result, err)
}

func buildGraphWriteResponse(operation string, intent *ebsv1.BuildGraph, result runtime.Object, err error) (*ebsv1.BuildGraph, error) {
	if err != nil {
		return nil, err
	}
	graph, ok := result.(*ebsv1.BuildGraph)
	if !ok || !graphWriteMatches(graph, intent) {
		return nil, unknownWrite(operation, source.BuildGraphsGVR, fmt.Errorf("unexpected BuildGraph write response"))
	}
	return graph, nil
}

func (c *apiClient) GetJob(ctx context.Context, project, name string) (*ebsv1.Job, error) {
	return c.getJob(ctx, source.JobsGVR, project, name)
}

func (c *apiClient) GetOldJob(ctx context.Context, project, name string) (*ebsv1.Job, error) {
	return c.getJob(ctx, ebsv1.SchemeGroupVersion.WithResource("oldjobs"), project, name)
}

func (c *apiClient) getJob(ctx context.Context, gvr schema.GroupVersionResource, project, name string) (*ebsv1.Job, error) {
	if project == "" || name == "" {
		return nil, contractErrorf("project and name are required")
	}
	obj, err := c.readGet(ctx, gvr, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Job)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected %s response for %s/%s: %T", gvr.Resource, project, name, obj)
	}
	return value, nil
}

func (c *apiClient) ListJobs(ctx context.Context, project string, selector labels.Selector, includeFields ...string) ([]ebsv1.Job, error) {
	return c.listJobs(ctx, source.JobsGVR, project, selector, includeFields...)
}

func (c *apiClient) ListOldJobs(ctx context.Context, project string, selector labels.Selector, includeFields ...string) ([]ebsv1.Job, error) {
	return c.listJobs(ctx, ebsv1.SchemeGroupVersion.WithResource("oldjobs"), project, selector, includeFields...)
}

func (c *apiClient) listJobs(ctx context.Context, gvr schema.GroupVersionResource, project string,
	selector labels.Selector, includeFields ...string) ([]ebsv1.Job, error) {
	if project == "" {
		return nil, contractErrorf("project is required")
	}
	if selector == nil {
		selector = labels.Everything()
	}
	var out []ebsv1.Job
	opts := metav1.ListOptions{Limit: 500, LabelSelector: selector.String()}
	for {
		page, err := c.readListProjectPage(ctx, gvr, project, opts, strings.Join(includeFields, ","))
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			job, ok := item.(*ebsv1.Job)
			if !ok || job == nil || job.UID == "" || job.ResourceVersion == "" ||
				job.Namespace != project || job.Name == "" || !selector.Matches(labels.Set(job.Labels)) {
				return nil, contractErrorf("unexpected %s list response for %s: %T", gvr.Resource, project, item)
			}
			out = append(out, *job)
		}
		if page.Continue == "" {
			return out, nil
		}
		if page.Continue == opts.Continue {
			return nil, contractErrorf("%s pagination did not advance", gvr.Resource)
		}
		opts.Continue = page.Continue
	}
}

func (c *apiClient) GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error) {
	if project == "" || name == "" {
		return nil, contractErrorf("project and name are required")
	}
	obj, err := c.readGet(ctx, source.BuildsGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Build)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Build response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetRpmRepo(ctx context.Context, project, name string) (*ebsv1.RpmRepo, error) {
	if project == "" || name == "" {
		return nil, contractErrorf("project and name are required")
	}
	obj, err := c.readGet(ctx, source.RpmReposGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.RpmRepo)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected RpmRepo response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetSnapshot(
	ctx context.Context, project, name string, includeFields ...string,
) (*ebsv1.Snapshot, error) {
	if project == "" || name == "" {
		return nil, contractErrorf("project and name are required")
	}
	obj, err := c.readGet(ctx, source.SnapshotsGVR, project, name, includeFields...)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Snapshot)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Snapshot response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetProject(ctx context.Context, project string) (*ebsv1.Project, error) {
	if project == "" {
		return nil, contractErrorf("project is required")
	}
	obj, err := c.readGet(ctx, source.ProjectsGVR, "", project, "metadata,status.phase")
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Project)
	if !ok || value == nil || value.Name != project || value.Namespace != "" || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Project response for %s: %T", project, obj)
	}
	return value, nil
}

func (c *apiClient) GetScript(ctx context.Context, name string) (*ebsv1.Script, error) {
	if reasons := kvalidation.IsDNS1123Subdomain(name); len(reasons) > 0 {
		return nil, contractErrorf("invalid Script name %q: %s", name, strings.Join(reasons, ", "))
	}
	obj, err := c.readGet(ctx, source.ScriptsGVR, "", name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Script)
	if !ok || value == nil || value.Name != name || value.Namespace != "" || value.UID == "" ||
		value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Script response for %s: %T", name, obj)
	}
	return value, nil
}

func (c *apiClient) GetBuildResourceContent(ctx context.Context) (*ebsv1.BuildResourceContent, error) {
	obj, err := c.readGet(ctx, source.ConfigsGVR, "", ebsv1.BuildResourceConfigName)
	if err != nil {
		return nil, err
	}
	config, ok := obj.(*ebsv1.Config)
	if !ok || config == nil || config.Name != ebsv1.BuildResourceConfigName || config.Namespace != "" ||
		config.UID == "" ||
		config.ResourceVersion == "" {
		return nil, contractErrorf("unexpected build-resource Config response: %T", obj)
	}
	var content ebsv1.BuildResourceContent
	if err := yaml.UnmarshalStrict([]byte(config.Spec.Content), &content); err != nil {
		return nil, fmt.Errorf("decode build-resource Config: %w", err)
	}
	return &content, nil
}

// readGet performs a Get with the fixed in-process retry for transient failures (network/timeout/5xx) and normalizes
// 404 to ErrNotFound.
func (c *apiClient) readGet(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	namespace, name string,
	includeFields ...string,
) (runtime.Object, error) {
	var obj runtime.Object
	var err error
	for attempt := 0; ; attempt++ {
		fields := ""
		if len(includeFields) > 0 {
			fields = includeFields[0]
		}
		obj, err = c.client.GetWithFields(ctx, gvr, namespace, name, fields)
		if err == nil {
			return obj, nil
		}
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		if ctx.Err() != nil || !isTransientReadError(err) || attempt >= apiServerReadRetries {
			return nil, err
		}
	}
}

// readListProjectPage performs a project-scoped list page fetch with the same fixed in-process retry for transient
// failures.
func (c *apiClient) readListProjectPage(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	project string,
	opts metav1.ListOptions,
	includeFields string,
) (source.ListPage, error) {
	var page source.ListPage
	var err error
	for attempt := 0; ; attempt++ {
		page, err = c.client.ListProjectPageWithFields(ctx, gvr, project, opts, includeFields)
		if err == nil {
			return page, nil
		}
		if ctx.Err() != nil || !isTransientReadError(err) || attempt >= apiServerReadRetries {
			return source.ListPage{}, err
		}
	}
}

// isTransientReadError reports whether a read failure is transient and worth an in-process retry: transport-level
// errors (no API status attached) plus server-side timeouts and 5xx. 404 and other 4xx are deterministic.
func isTransientReadError(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsInternalError(err)
}

func notSentWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &apiserver.WriteError{
		Operation: operation,
		Resource:  gvr.GroupResource(),
		Outcome:   apiserver.WriteNotSent,
		Err:       err,
	}
}

func unknownWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &apiserver.WriteError{
		Operation: operation,
		Resource:  gvr.GroupResource(),
		Outcome:   apiserver.WriteUnknown,
		Err:       err,
	}
}

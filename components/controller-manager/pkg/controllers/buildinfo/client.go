// client.go provides the typed API client used by the BuildInfo controller
// (design 4.1). It wraps the shared controller-manager API client, so write
// errors keep the shared NotSent / Rejected / Unknown classification, and
// read misses are normalized to the ErrNotFound sentinel.

package buildinfo

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

var configResourceArchPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
var configPackagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._-]*(:[A-Za-z0-9][A-Za-z0-9+._-]*)*$`)

// ErrNotFound is the sentinel returned by read methods when the requested
// object does not exist, so callers distinguish "missing" from "query failed".
var ErrNotFound = errors.New("object not found")

// apiServerReadRetries is the fixed in-process retry budget for transient
// apiserver query failures (network errors, timeouts, 5xx). It is a code
// constant by design, unlike the configurable git-server retry (7.5/E-02).
const apiServerReadRetries = 3

// SharedClient is the shared client surface this typed client needs: the
// generic CRUD interface plus the build-target Config reader.
type SharedClient interface {
	apiserver.Interface
	GetBuildTargetContent(ctx context.Context) (*ebsv1.BuildTargetContent, error)
}

// buildResourceRules keeps the Config identity needed by Job annotations
// together with its parsed resource rules; it is not an API resource.
type buildResourceRules struct {
	metav1.ObjectMeta
	Spec ebsv1.BuildResourceContent
}

func (r *buildResourceRules) DeepCopy() *buildResourceRules {
	if r == nil {
		return nil
	}
	out := *r
	out.ObjectMeta = *r.ObjectMeta.DeepCopy()
	out.Spec = *r.Spec.DeepCopy()
	return &out
}

var _ SharedClient = (*apiserver.Client)(nil)

// Client is the typed API surface the BuildInfo controller depends on.
//
// BuildInfo status writes go through /status; the apiserver keeps the old
// spec. specDepends is an in-memory view and is never persisted, so there is
// no PUT on the main resource. Write failures are returned as *WriteError
// identifiable via errors.As; the controller never re-derives the outcome
// from status codes or error text.
type Client interface {
	// GetBuildInfo re-gets the latest BuildInfo (reconcile entry, E-10).
	GetBuildInfo(ctx context.Context, namespace, name string) (*ebsv1.BuildInfo, error)
	// UpdateBuildInfoStatus writes BuildInfo.status via /status with the
	// object's resourceVersion as an optimistic lock.
	UpdateBuildInfoStatus(ctx context.Context, obj *ebsv1.BuildInfo) (*ebsv1.BuildInfo, error)

	// CreateJob creates a Job (naming/label contract 15.3.1). Unknown
	// outcomes are confirmed by GET on the deterministic Job name (10.3/E-11).
	CreateJob(ctx context.Context, project string, obj *ebsv1.Job) (*ebsv1.Job, error)
	GetJob(ctx context.Context, project, name string) (*ebsv1.Job, error)
	ListJobs(ctx context.Context, project string, selector labels.Selector) ([]ebsv1.Job, error)

	// GetBuild is read-only (G-01: Builds are never mutated here).
	GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error)
	GetRpmRepo(ctx context.Context, project, name string) (*ebsv1.RpmRepo, error)
	GetSnapshot(ctx context.Context, project, name string) (*ebsv1.Snapshot, error)
	GetProject(ctx context.Context, project string) (*ebsv1.Project, error)
	// GetBuildResourceRules reads the cluster-wide default resource table.
	GetBuildResourceRules(ctx context.Context) (*buildResourceRules, error)
	// GetBuildTargetContent returns a snapshot for one Job creation batch. Callers
	// must not refetch it per Job, or use it to mutate a created Job (E-26).
	GetBuildTargetContent(ctx context.Context) (*ebsv1.BuildTargetContent, error)
}

// newAPIClient wraps the shared controller-manager API client.
func newAPIClient(client SharedClient) Client { return &apiClient{client: client} }

type apiClient struct{ client SharedClient }

var _ Client = (*apiClient)(nil)

// contractError reports a request or response contract violation: invalid
// client input, or a server response that does not match the requested
// resource identity. Retrying cannot fix it, so read paths classify it as a
// permanent error instead of entering the retry backoff.
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
	if !ok || value == nil || value.Name != name || value.Namespace != namespace || value.UID == "" || value.ResourceVersion == "" {
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
	if !ok || value == nil || value.UID != obj.UID || value.Name != obj.Name || value.Namespace != obj.Namespace || value.ResourceVersion == "" {
		return nil, unknownWrite("update-status", source.BuildInfosGVR, fmt.Errorf("unexpected BuildInfo status response: %T", updated))
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
	if !ok || value == nil || value.Name != obj.Name || value.Namespace != obj.Namespace || value.UID == "" || value.ResourceVersion == "" {
		return nil, unknownWrite("create", source.JobsGVR, fmt.Errorf("unexpected Job create response: %T", created))
	}
	return value, nil
}

func (c *apiClient) GetJob(ctx context.Context, project, name string) (*ebsv1.Job, error) {
	if project == "" || name == "" {
		return nil, contractErrorf("project and name are required")
	}
	obj, err := c.readGet(ctx, source.JobsGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Job)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Job response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) ListJobs(ctx context.Context, project string, selector labels.Selector) ([]ebsv1.Job, error) {
	if project == "" {
		return nil, contractErrorf("project is required")
	}
	if selector == nil {
		selector = labels.Everything()
	}
	var out []ebsv1.Job
	opts := metav1.ListOptions{LabelSelector: selector.String()}
	for {
		page, err := c.readListProjectPage(ctx, source.JobsGVR, project, opts)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			job, ok := item.(*ebsv1.Job)
			if !ok || job == nil || job.UID == "" || job.ResourceVersion == "" {
				return nil, contractErrorf("unexpected Job list response for %s: %T", project, item)
			}
			out = append(out, *job)
		}
		if page.Continue == "" {
			return out, nil
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
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
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
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected RpmRepo response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetSnapshot(ctx context.Context, project, name string) (*ebsv1.Snapshot, error) {
	if project == "" || name == "" {
		return nil, contractErrorf("project and name are required")
	}
	obj, err := c.readGet(ctx, source.SnapshotsGVR, project, name)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Snapshot)
	if !ok || value == nil || value.Name != name || value.Namespace != project || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Snapshot response for %s/%s: %T", project, name, obj)
	}
	return value, nil
}

func (c *apiClient) GetProject(ctx context.Context, project string) (*ebsv1.Project, error) {
	if project == "" {
		return nil, contractErrorf("project is required")
	}
	obj, err := c.readGet(ctx, source.ProjectsGVR, "", project)
	if err != nil {
		return nil, err
	}
	value, ok := obj.(*ebsv1.Project)
	if !ok || value == nil || value.Name != project || value.Namespace != "" || value.UID == "" || value.ResourceVersion == "" {
		return nil, contractErrorf("unexpected Project response for %s: %T", project, obj)
	}
	return value, nil
}

func (c *apiClient) GetBuildResourceRules(ctx context.Context) (*buildResourceRules, error) {
	obj, err := c.readGet(ctx, source.ConfigsGVR, "", ebsv1.BuildResourceConfigName)
	if err != nil {
		return nil, err
	}
	config, ok := obj.(*ebsv1.Config)
	if !ok || config == nil || config.Name != ebsv1.BuildResourceConfigName || config.Namespace != "" || config.UID == "" || config.ResourceVersion == "" {
		return nil, contractErrorf("unexpected build-resource Config response: %T", obj)
	}
	var content ebsv1.BuildResourceContent
	if err := yaml.UnmarshalStrict([]byte(config.Spec.Content), &content); err != nil {
		return nil, fmt.Errorf("decode build-resource Config: %w", err)
	}
	if content.Default.Requests["cpu"] == "" || content.Default.Requests["memory"] == "" {
		return nil, fmt.Errorf("build-resource Config requires default CPU and memory requests")
	}
	if err := validateConfigResourceLevel(content.Default); err != nil {
		return nil, fmt.Errorf("invalid build-resource default: %w", err)
	}
	parsed := &buildResourceRules{ObjectMeta: config.ObjectMeta, Spec: content}
	if err := validateEffectiveResources(resolveResources(parsed, "", "")); err != nil {
		return nil, err
	}
	for name, pkg := range content.Packages {
		if !configPackagePattern.MatchString(name) || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("invalid build-resource package name %q", name)
		}
		if len(pkg.Default.Requests) == 0 && len(pkg.Default.Limits) == 0 && len(pkg.Arches) == 0 {
			return nil, fmt.Errorf("package %q has no resource rules", name)
		}
		if err := validateConfigResourceLevel(pkg.Default); err != nil {
			return nil, fmt.Errorf("invalid package %q default: %w", name, err)
		}
		if err := validateEffectiveResources(resolveResources(parsed, name, "")); err != nil {
			return nil, fmt.Errorf("invalid package %q resources: %w", name, err)
		}
		for arch, level := range pkg.Arches {
			if !configResourceArchPattern.MatchString(arch) {
				return nil, fmt.Errorf("invalid build-resource architecture %q", arch)
			}
			if err := validateConfigResourceLevel(level); err != nil {
				return nil, fmt.Errorf("invalid package %q architecture %q: %w", name, arch, err)
			}
			if err := validateEffectiveResources(resolveResources(parsed, name, arch)); err != nil {
				return nil, fmt.Errorf("invalid package %q architecture %q resources: %w", name, arch, err)
			}
		}
	}
	return parsed, nil
}

func validateEffectiveResources(level ebsv1.ResourceRequirements) error {
	for _, key := range []string{"cpu", "memory"} {
		request, reqErr := resource.ParseQuantity(level.Requests[key])
		limit, limitErr := resource.ParseQuantity(level.Limits[key])
		if reqErr != nil || limitErr != nil || request.Sign() <= 0 || limit.Cmp(request) < 0 {
			return fmt.Errorf("invalid effective %s requests/limits", key)
		}
	}
	return nil
}

func validateConfigResourceLevel(level ebsv1.ResourceRequirements) error {
	for _, values := range []map[string]string{level.Requests, level.Limits} {
		for key, value := range values {
			if key != "cpu" && key != "memory" {
				return fmt.Errorf("unsupported resource %q", key)
			}
			quantity, err := resource.ParseQuantity(value)
			if err != nil || quantity.Sign() <= 0 {
				return fmt.Errorf("invalid %s quantity %q", key, value)
			}
		}
	}
	return nil
}

func (c *apiClient) GetBuildTargetContent(ctx context.Context) (*ebsv1.BuildTargetContent, error) {
	var conf *ebsv1.BuildTargetContent
	var err error
	for attempt := 0; ; attempt++ {
		conf, err = c.client.GetBuildTargetContent(ctx)
		if err == nil {
			break
		}
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		if ctx.Err() != nil || !isTransientReadError(err) || attempt >= apiServerReadRetries {
			return nil, err
		}
	}
	if conf == nil || conf.Targets == nil {
		return nil, contractErrorf("unexpected build-target content: %T", conf)
	}
	return conf, nil
}

// readGet performs a Get with the fixed in-process retry for transient
// failures (network/timeout/5xx) and normalizes 404 to ErrNotFound.
func (c *apiClient) readGet(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error) {
	var obj runtime.Object
	var err error
	for attempt := 0; ; attempt++ {
		obj, err = c.client.Get(ctx, gvr, namespace, name)
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

// readListProjectPage performs a project-scoped list page fetch with the same
// fixed in-process retry for transient failures.
func (c *apiClient) readListProjectPage(ctx context.Context, gvr schema.GroupVersionResource, project string, opts metav1.ListOptions) (source.ListPage, error) {
	var page source.ListPage
	var err error
	for attempt := 0; ; attempt++ {
		page, err = c.client.ListProjectPage(ctx, gvr, project, opts)
		if err == nil {
			return page, nil
		}
		if ctx.Err() != nil || !isTransientReadError(err) || attempt >= apiServerReadRetries {
			return source.ListPage{}, err
		}
	}
}

// isTransientReadError reports whether a read failure is transient and worth
// an in-process retry: transport-level errors (no API status attached) plus
// server-side timeouts and 5xx. 404 and other 4xx are deterministic.
func isTransientReadError(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}
	return apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsInternalError(err)
}

func notSentWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &apiserver.WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: apiserver.WriteNotSent, Err: err}
}

func unknownWrite(operation string, gvr schema.GroupVersionResource, err error) error {
	return &apiserver.WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: apiserver.WriteUnknown, Err: err}
}

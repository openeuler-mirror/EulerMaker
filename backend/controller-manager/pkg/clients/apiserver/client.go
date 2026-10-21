package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

type DeletePreconditions struct {
	UID             types.UID
	ResourceVersion string
}

type WriteOutcome string

const (
	WriteNotSent  WriteOutcome = "NotSent"
	WriteRejected WriteOutcome = "Rejected"
	WriteUnknown  WriteOutcome = "Unknown"
)

type WriteError struct {
	Operation  string
	Resource   schema.GroupResource
	Outcome    WriteOutcome
	StatusCode int
	RetryAfter time.Duration
	Err        error
}

// Error describes the failed write operation and its outcome.
func (e *WriteError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s %s failed (%s): %v", e.Operation, e.Resource.String(), e.Outcome, e.Err)
}

// Unwrap exposes the underlying write error.
func (e *WriteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// RetryAfter returns the server retry delay for rejected writes with HTTP 429 or 503.
func RetryAfter(err error) time.Duration {
	var writeErr *WriteError
	if !errors.As(err, &writeErr) || writeErr.Outcome != WriteRejected || writeErr.RetryAfter <= 0 {
		return 0
	}
	if writeErr.StatusCode != 429 && writeErr.StatusCode != 503 {
		return 0
	}
	return writeErr.RetryAfter
}

type Client struct {
	rest      *rest.RESTClient
	scheme    *runtime.Scheme
	resources map[string]resourceKinds
	timeout   time.Duration
}

type resourceKinds struct {
	object schema.GroupVersionKind
	list   schema.GroupVersionKind
}

type Interface interface {
	// ListPage reads one cluster-wide page with resource-specific polling or history-cleanup field projections.
	ListPage(context.Context, schema.GroupVersionResource, metav1.ListOptions) (source.ListPage, error)
	// ListProjectPage reads one page of complete objects within the specified Project.
	ListProjectPage(context.Context, schema.GroupVersionResource, string, metav1.ListOptions) (source.ListPage, error)
	// ResolveWatch provides a list/watch source for nonterminal Jobs or all Runners.
	ResolveWatch(context.Context, schema.GroupVersionResource) (source.WatchResource, error)
	// Get reads a complete object by name and optional Project namespace.
	Get(context.Context, schema.GroupVersionResource, string, string) (runtime.Object, error)
	// GetWithFields reads a complete object or a read-only projection when includeFields is nonempty.
	GetWithFields(context.Context, schema.GroupVersionResource, string, string, string) (runtime.Object, error)
	// PostSubresource sends JSON to a subresource and returns an object of the parent resource type.
	PostSubresource(context.Context, schema.GroupVersionResource, string, string, string, any) (runtime.Object, error)
	// Create posts a new object and returns the server-assigned metadata.
	Create(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error)
	// Update puts an object on its main resource endpoint using its resourceVersion.
	Update(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error)
	// UpdateStatus puts an object on its status subresource using its resourceVersion.
	UpdateStatus(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error)
	// Delete removes an object with required UID and resourceVersion preconditions.
	Delete(context.Context, schema.GroupVersionResource, string, string, DeletePreconditions) error
}

var _ Interface = (*Client)(nil)

// New creates an EBS API client with a per-request timeout.
func New(config *rest.Config, timeout time.Duration) (*Client, error) {
	if config == nil || timeout <= 0 {
		return nil, fmt.Errorf("REST config and positive timeout are required")
	}
	scheme := runtime.NewScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		return nil, err
	}
	if err := ebsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	resources := make(map[string]resourceKinds)
	for _, resource := range ebsv1.ResourceRegistrations() {
		objectKinds, _, err := scheme.ObjectKinds(resource.Object)
		if err != nil {
			return nil, err
		}
		listKinds, _, err := scheme.ObjectKinds(resource.List)
		if err != nil {
			return nil, err
		}
		resources[resource.Resource] = resourceKinds{object: objectKinds[0], list: listKinds[0]}
	}
	cfg := rest.CopyConfig(config)
	cfg.GroupVersion = &ebsv1.SchemeGroupVersion
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()
	if cfg.UserAgent == "" {
		cfg.UserAgent = "eulermaker-controller-manager/dev"
	}
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{rest: rc, scheme: scheme, resources: resources, timeout: timeout}, nil
}

// ListPage reads one cluster-wide page with resource-specific polling or history-cleanup field projections.
func (c *Client) ListPage(ctx context.Context, gvr schema.GroupVersionResource, opts metav1.ListOptions) (source.ListPage, error) {
	if gvr == source.JobsGVR {
		return c.listPage(ctx, gvr, "", opts, "metadata,status.phase,status.endTime")
	}
	// Polling sources only need the fields used to enqueue resource keys.
	return c.listPage(ctx, gvr, "", opts, source.PollingIncludeFields(gvr))
}

// ListProjectPage reads one page of complete objects within the specified Project.
func (c *Client) ListProjectPage(ctx context.Context, gvr schema.GroupVersionResource, project string, opts metav1.ListOptions) (source.ListPage, error) {
	if err := c.validateProjectScopedResource(gvr, project); err != nil {
		return source.ListPage{}, err
	}
	return c.listPage(ctx, gvr, project, opts, "")
}

func (c *Client) listPage(ctx context.Context, gvr schema.GroupVersionResource, project string, opts metav1.ListOptions, includeFields string) (source.ListPage, error) {
	list, err := c.newList(gvr)
	if err != nil {
		return source.ListPage{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request := c.rest.Get().AbsPath(resourcePath(gvr, project, "", "")).VersionedParams(&opts, metav1.ParameterCodec)
	if includeFields != "" {
		request.Param("includeFields", includeFields)
	}
	if err := request.Do(requestCtx).Into(list); err != nil {
		return source.ListPage{}, err
	}
	return listPage(list)
}

// ArchiveJob persists a snapshot without deleting its source.
func (c *Client) ArchiveJob(ctx context.Context, project, name string, pre DeletePreconditions) (*ebsv1.Job, error) {
	gvr := source.JobsGVR
	if err := c.validateTarget(gvr, project, name); err != nil {
		return nil, notSent("archive", gvr, err)
	}
	if pre.UID == "" || pre.ResourceVersion == "" {
		return nil, notSent("archive", gvr, fmt.Errorf("Job UID and resourceVersion are required"))
	}
	body, err := json.Marshal(struct {
		Preconditions metav1.Preconditions `json:"preconditions"`
	}{
		Preconditions: metav1.Preconditions{UID: &pre.UID, ResourceVersion: &pre.ResourceVersion},
	})
	if err != nil {
		return nil, notSent("archive", gvr, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	out := &ebsv1.Job{}
	if err := c.rest.Post().AbsPath(resourcePath(gvr, project, name, "archive")).Body(body).Do(requestCtx).Into(out); err != nil {
		return nil, classifyWrite("archive", gvr, err)
	}
	if out.Namespace != project || out.Name != name || out.UID != pre.UID || out.ResourceVersion != pre.ResourceVersion {
		return nil, unknown("archive", gvr, fmt.Errorf("unexpected Job archive response identity or version"))
	}
	return out, nil
}

// PostSubresource posts a JSON request and decodes a response of the parent resource type.
func (c *Client) PostSubresource(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, subresource string, body any) (runtime.Object, error) {
	const operation = "post-subresource"
	if err := c.validateTarget(gvr, namespace, name); err != nil {
		return nil, notSent(operation, gvr, err)
	}
	if reasons := kvalidation.IsDNS1123Label(subresource); len(reasons) != 0 {
		return nil, notSent(operation, gvr, fmt.Errorf("invalid subresource %q: %v", subresource, reasons))
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, notSent(operation, gvr, err)
	}
	out, err := c.newObject(gvr)
	if err != nil {
		return nil, notSent(operation, gvr, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rest.Post().AbsPath(resourcePath(gvr, namespace, name, subresource)).Body(data).
		Do(requestCtx).Into(out); err != nil {
		return nil, classifyWrite(operation, gvr, err)
	}
	if err := validateResponseObject(out); err != nil {
		return nil, unknown(operation, gvr, err)
	}
	accessor, err := apiMeta.Accessor(out)
	if err != nil || accessor.GetName() != name || accessor.GetNamespace() != namespace {
		return nil, unknown(operation, gvr, fmt.Errorf("response object identity does not match request"))
	}
	return out, nil
}

// Create posts a new object and returns the server-assigned metadata.
func (c *Client) Create(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj runtime.Object) (runtime.Object, error) {
	if obj == nil {
		return nil, notSent("create", gvr, fmt.Errorf("object is required"))
	}
	accessor, err := apiMeta.Accessor(obj)
	if err != nil {
		return nil, notSent("create", gvr, err)
	}
	if err := c.validateTarget(gvr, namespace, accessor.GetName()); err != nil {
		return nil, notSent("create", gvr, err)
	}
	if accessor.GetNamespace() != namespace || accessor.GetUID() != "" || accessor.GetResourceVersion() != "" {
		return nil, notSent("create", gvr, fmt.Errorf("object metadata does not match target or contains server-assigned UID/resourceVersion"))
	}
	out, err := c.newObject(gvr)
	if err != nil {
		return nil, notSent("create", gvr, err)
	}
	if reflect.TypeOf(obj) != reflect.TypeOf(out) {
		return nil, notSent("create", gvr, fmt.Errorf("object type %T does not match resource %s", obj, gvr.Resource))
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	err = c.rest.Post().AbsPath(resourcePath(gvr, namespace, "", "")).Body(obj).Do(requestCtx).Into(out)
	if err != nil {
		return nil, classifyWrite("create", gvr, err)
	}
	if err := validateResponseObject(out); err != nil {
		return nil, unknown("create", gvr, err)
	}
	responseAccessor, err := apiMeta.Accessor(out)
	if err != nil || responseAccessor.GetName() != accessor.GetName() || responseAccessor.GetNamespace() != accessor.GetNamespace() {
		return nil, unknown("create", gvr, fmt.Errorf("response object identity does not match request"))
	}
	return out, nil
}

// ResolveWatch provides a list/watch source for nonterminal Jobs or all Runners.
func (c *Client) ResolveWatch(ctx context.Context, gvr schema.GroupVersionResource) (source.WatchResource, error) {
	if gvr != source.JobsGVR && gvr != source.RunnersGVR {
		return source.WatchResource{}, source.ErrWatchUnsupported
	}
	objectType := runtime.Object(&ebsv1.Job{})
	if gvr == source.RunnersGVR {
		objectType = &ebsv1.Runner{}
	}
	lw := &listerWatcher{ctx: ctx, client: c, gvr: gvr}
	if gvr == source.JobsGVR {
		lw.fieldSelector = "status.phase!=Succeeded,status.phase!=Failed,status.phase!=Aborted"
	}
	return source.WatchResource{ListerWatcher: lw, ObjectType: objectType}, nil
}

// Get reads a complete object by name and optional Project namespace.
func (c *Client) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error) {
	return c.GetWithFields(ctx, gvr, namespace, name, "")
}

// GetWithFields returns a read-only projection when includeFields is nonempty.
func (c *Client) GetWithFields(ctx context.Context, gvr schema.GroupVersionResource, namespace, name, includeFields string) (runtime.Object, error) {
	if err := c.validateTarget(gvr, namespace, name); err != nil {
		return nil, err
	}
	out, err := c.newObject(gvr)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request := c.rest.Get().AbsPath(resourcePath(gvr, namespace, name, ""))
	if includeFields != "" {
		request.Param("includeFields", includeFields)
	}
	err = request.Do(requestCtx).Into(out)
	if err != nil {
		return nil, err
	}
	if err := validateResponseObject(out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateStatus puts an object on its status subresource using its resourceVersion.
func (c *Client) UpdateStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj runtime.Object) (runtime.Object, error) {
	return c.update(ctx, "update-status", gvr, namespace, "status", obj)
}

// Update puts an object on its main resource endpoint using its resourceVersion.
func (c *Client) Update(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj runtime.Object) (runtime.Object, error) {
	return c.update(ctx, "update", gvr, namespace, "", obj)
}

func (c *Client) update(ctx context.Context, operation string, gvr schema.GroupVersionResource, namespace, subresource string, obj runtime.Object) (runtime.Object, error) {
	if obj == nil {
		return nil, notSent(operation, gvr, fmt.Errorf("object is required"))
	}
	accessor, err := apiMeta.Accessor(obj)
	if err != nil {
		return nil, notSent(operation, gvr, err)
	}
	if err := c.validateTarget(gvr, namespace, accessor.GetName()); err != nil {
		return nil, notSent(operation, gvr, err)
	}
	if accessor.GetNamespace() != namespace || accessor.GetUID() == "" || accessor.GetResourceVersion() == "" {
		return nil, notSent(operation, gvr, fmt.Errorf("object metadata does not match target or lacks UID/resourceVersion"))
	}
	out, err := c.newObject(gvr)
	if err != nil {
		return nil, notSent(operation, gvr, err)
	}
	if reflect.TypeOf(obj) != reflect.TypeOf(out) {
		return nil, notSent(operation, gvr, fmt.Errorf("object type %T does not match resource %s", obj, gvr.Resource))
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	err = c.rest.Put().AbsPath(resourcePath(gvr, namespace, accessor.GetName(), subresource)).Body(obj).Do(requestCtx).Into(out)
	if err != nil {
		return nil, classifyWrite(operation, gvr, err)
	}
	if err := validateResponseObject(out); err != nil {
		return nil, unknown(operation, gvr, err)
	}
	responseAccessor, err := apiMeta.Accessor(out)
	if err != nil || responseAccessor.GetUID() != accessor.GetUID() || responseAccessor.GetName() != accessor.GetName() || responseAccessor.GetNamespace() != accessor.GetNamespace() {
		return nil, unknown(operation, gvr, fmt.Errorf("response object identity does not match request"))
	}
	return out, nil
}

// Delete removes an object with required UID and resourceVersion preconditions.
func (c *Client) Delete(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, preconditions DeletePreconditions) error {
	if err := c.validateTarget(gvr, namespace, name); err != nil {
		return notSent("delete", gvr, err)
	}
	if preconditions.UID == "" || preconditions.ResourceVersion == "" {
		return notSent("delete", gvr, fmt.Errorf("UID and resourceVersion preconditions are required"))
	}
	opts := &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &preconditions.UID, ResourceVersion: &preconditions.ResourceVersion}}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rest.Delete().AbsPath(resourcePath(gvr, namespace, name, "")).Body(opts).Do(requestCtx).Error(); err != nil {
		return classifyWrite("delete", gvr, err)
	}
	return nil
}

func (c *Client) validateTarget(gvr schema.GroupVersionResource, namespace, name string) error {
	if _, err := c.resourceKinds(gvr); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("resource name is required")
	}
	clusterScoped := gvr.Resource == "projects" || gvr.Resource == "runners" || gvr.Resource == "configs" || gvr.Resource == "scripts"
	if clusterScoped && namespace != "" {
		return fmt.Errorf("cluster-scoped resource %s requires an empty namespace", gvr.Resource)
	}
	if !clusterScoped && namespace == "" {
		return fmt.Errorf("namespace-scoped resource %s requires a namespace", gvr.Resource)
	}
	return nil
}

func (c *Client) validateProjectScopedResource(gvr schema.GroupVersionResource, project string) error {
	if _, err := c.resourceKinds(gvr); err != nil {
		return err
	}
	if project == "" {
		return fmt.Errorf("project is required")
	}
	if gvr.Resource == "projects" || gvr.Resource == "runners" || gvr.Resource == "configs" || gvr.Resource == "scripts" {
		return fmt.Errorf("resource %s is not project-scoped", gvr.Resource)
	}
	return nil
}

func resourcePath(gvr schema.GroupVersionResource, namespace, name, subresource string) string {
	base := "/apis/" + gvr.Group + "/" + gvr.Version + "/"
	if namespace != "" {
		base += "projects/" + namespace + "/"
	}
	base += gvr.Resource
	if name != "" {
		base += "/" + name
	}
	if subresource != "" {
		base += "/" + subresource
	}
	return base
}

func validateResponseObject(obj runtime.Object) error {
	if obj == nil {
		return fmt.Errorf("response object is nil")
	}
	accessor, err := apiMeta.Accessor(obj)
	if err != nil {
		return err
	}
	if accessor.GetUID() == "" || accessor.GetResourceVersion() == "" {
		return fmt.Errorf("response object lacks UID/resourceVersion")
	}
	return nil
}

func notSent(operation string, gvr schema.GroupVersionResource, err error) *WriteError {
	return &WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: WriteNotSent, Err: err}
}
func unknown(operation string, gvr schema.GroupVersionResource, err error) *WriteError {
	return &WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: WriteUnknown, Err: err}
}
func classifyWrite(operation string, gvr schema.GroupVersionResource, err error) *WriteError {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := int(status.Status().Code)
		result := &WriteError{Operation: operation, Resource: gvr.GroupResource(), Outcome: WriteRejected, StatusCode: code, Err: err}
		if seconds, ok := apierrors.SuggestsClientDelay(err); ok && seconds > 0 {
			result.RetryAfter = time.Duration(seconds) * time.Second
		}
		return result
	}
	return unknown(operation, gvr, err)
}

type listerWatcher struct {
	ctx           context.Context
	client        *Client
	gvr           schema.GroupVersionResource
	fieldSelector string
}

func (l *listerWatcher) options(opts metav1.ListOptions) metav1.ListOptions {
	if l.fieldSelector != "" {
		if opts.FieldSelector != "" {
			opts.FieldSelector += ","
		}
		opts.FieldSelector += l.fieldSelector
	}
	return opts
}

// List loads objects with the source's field selector for the informer cache.
func (l *listerWatcher) List(opts metav1.ListOptions) (runtime.Object, error) {
	opts = l.options(opts)
	list, err := l.client.newList(l.gvr)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(l.ctx, l.client.timeout)
	defer cancel()
	err = l.client.rest.Get().AbsPath("/apis/"+l.gvr.Group+"/"+l.gvr.Version+"/"+l.gvr.Resource).VersionedParams(&opts, metav1.ParameterCodec).Do(requestCtx).Into(list)
	return list, err
}

// Watch streams object changes with the source's field selector.
func (l *listerWatcher) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	opts = l.options(opts)
	opts.Watch = true
	return l.client.rest.Get().AbsPath("/apis/"+l.gvr.Group+"/"+l.gvr.Version+"/"+l.gvr.Resource).VersionedParams(&opts, metav1.ParameterCodec).Watch(l.ctx)
}

var _ cache.ListerWatcher = (*listerWatcher)(nil)

func (c *Client) resourceKinds(gvr schema.GroupVersionResource) (resourceKinds, error) {
	kinds, ok := c.resources[gvr.Resource]
	if gvr.GroupVersion() != ebsv1.SchemeGroupVersion || !ok {
		return resourceKinds{}, fmt.Errorf("unsupported resource %s", gvr)
	}
	return kinds, nil
}

func (c *Client) newList(gvr schema.GroupVersionResource) (runtime.Object, error) {
	kinds, err := c.resourceKinds(gvr)
	if err != nil {
		return nil, err
	}
	return c.scheme.New(kinds.list)
}

func (c *Client) newObject(gvr schema.GroupVersionResource) (runtime.Object, error) {
	kinds, err := c.resourceKinds(gvr)
	if err != nil {
		return nil, err
	}
	return c.scheme.New(kinds.object)
}

func listPage(list runtime.Object) (source.ListPage, error) {
	accessor, err := apiMeta.ListAccessor(list)
	if err != nil {
		return source.ListPage{}, fmt.Errorf("read list metadata: %w", err)
	}
	items, err := apiMeta.ExtractList(list)
	if err != nil {
		return source.ListPage{}, fmt.Errorf("extract list items: %w", err)
	}
	page := source.ListPage{
		Continue:        accessor.GetContinue(),
		ResourceVersion: accessor.GetResourceVersion(),
	}
	for _, item := range items {
		page.Items = append(page.Items, item.DeepCopyObject())
	}
	return page, nil
}

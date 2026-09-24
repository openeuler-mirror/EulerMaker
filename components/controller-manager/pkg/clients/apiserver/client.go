package apiserver

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
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

func (e *WriteError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s %s failed (%s): %v", e.Operation, e.Resource.String(), e.Outcome, e.Err)
}
func (e *WriteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

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
	rest    *rest.RESTClient
	timeout time.Duration
}

type Interface interface {
	ListPage(context.Context, schema.GroupVersionResource, metav1.ListOptions) (source.ListPage, error)
	ListProjectPage(context.Context, schema.GroupVersionResource, string, metav1.ListOptions) (source.ListPage, error)
	ResolveWatch(context.Context, schema.GroupVersionResource) (source.WatchResource, error)
	Get(context.Context, schema.GroupVersionResource, string, string) (runtime.Object, error)
	Create(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error)
	Update(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error)
	UpdateStatus(context.Context, schema.GroupVersionResource, string, runtime.Object) (runtime.Object, error)
	Delete(context.Context, schema.GroupVersionResource, string, string, DeletePreconditions) error
}

var _ Interface = (*Client)(nil)

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
	return &Client{rest: rc, timeout: timeout}, nil
}

func (c *Client) ListPage(ctx context.Context, gvr schema.GroupVersionResource, opts metav1.ListOptions) (source.ListPage, error) {
	return c.listPage(ctx, gvr, "", opts)
}

func (c *Client) ListProjectPage(ctx context.Context, gvr schema.GroupVersionResource, project string, opts metav1.ListOptions) (source.ListPage, error) {
	if err := validateProjectScopedResource(gvr, project); err != nil {
		return source.ListPage{}, err
	}
	return c.listPage(ctx, gvr, project, opts)
}

func (c *Client) listPage(ctx context.Context, gvr schema.GroupVersionResource, project string, opts metav1.ListOptions) (source.ListPage, error) {
	list, err := newList(gvr)
	if err != nil {
		return source.ListPage{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rest.Get().AbsPath(resourcePath(gvr, project, "", "")).VersionedParams(&opts, metav1.ParameterCodec).Do(requestCtx).Into(list); err != nil {
		return source.ListPage{}, err
	}
	return listPage(list)
}

func (c *Client) Create(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj runtime.Object) (runtime.Object, error) {
	if obj == nil {
		return nil, notSent("create", gvr, fmt.Errorf("object is required"))
	}
	accessor, err := apiMeta.Accessor(obj)
	if err != nil {
		return nil, notSent("create", gvr, err)
	}
	if err := validateTarget(gvr, namespace, accessor.GetName()); err != nil {
		return nil, notSent("create", gvr, err)
	}
	if accessor.GetNamespace() != namespace || accessor.GetUID() != "" || accessor.GetResourceVersion() != "" {
		return nil, notSent("create", gvr, fmt.Errorf("object metadata does not match target or contains server-assigned UID/resourceVersion"))
	}
	out, err := newObject(gvr)
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

func (c *Client) ResolveWatch(ctx context.Context, gvr schema.GroupVersionResource) (source.WatchResource, error) {
	if gvr != source.JobsGVR && gvr != source.RunnersGVR {
		return source.WatchResource{}, source.ErrWatchUnsupported
	}
	objectType := runtime.Object(&ebsv1.Job{})
	if gvr == source.RunnersGVR {
		objectType = &ebsv1.Runner{}
	}
	lw := &listerWatcher{ctx: ctx, client: c, gvr: gvr}
	return source.WatchResource{ListerWatcher: lw, ObjectType: objectType}, nil
}

func (c *Client) Get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error) {
	if err := validateTarget(gvr, namespace, name); err != nil {
		return nil, err
	}
	out, err := newObject(gvr)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	err = c.rest.Get().AbsPath(resourcePath(gvr, namespace, name, "")).Do(requestCtx).Into(out)
	if err != nil {
		return nil, err
	}
	if err := validateResponseObject(out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) UpdateStatus(ctx context.Context, gvr schema.GroupVersionResource, namespace string, obj runtime.Object) (runtime.Object, error) {
	return c.update(ctx, "update-status", gvr, namespace, "status", obj)
}

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
	if err := validateTarget(gvr, namespace, accessor.GetName()); err != nil {
		return nil, notSent(operation, gvr, err)
	}
	if accessor.GetNamespace() != namespace || accessor.GetUID() == "" || accessor.GetResourceVersion() == "" {
		return nil, notSent(operation, gvr, fmt.Errorf("object metadata does not match target or lacks UID/resourceVersion"))
	}
	out, err := newObject(gvr)
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

func (c *Client) Delete(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, preconditions DeletePreconditions) error {
	if err := validateTarget(gvr, namespace, name); err != nil {
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

func validateTarget(gvr schema.GroupVersionResource, namespace, name string) error {
	if _, err := newObject(gvr); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("resource name is required")
	}
	clusterScoped := gvr.Resource == "projects" || gvr.Resource == "runners" || gvr.Resource == "configs"
	if clusterScoped && namespace != "" {
		return fmt.Errorf("cluster-scoped resource %s requires an empty namespace", gvr.Resource)
	}
	if !clusterScoped && namespace == "" {
		return fmt.Errorf("namespace-scoped resource %s requires a namespace", gvr.Resource)
	}
	return nil
}

func validateProjectScopedResource(gvr schema.GroupVersionResource, project string) error {
	if _, err := newList(gvr); err != nil {
		return err
	}
	if project == "" {
		return fmt.Errorf("project is required")
	}
	if gvr.Resource == "projects" || gvr.Resource == "runners" || gvr.Resource == "configs" {
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
	ctx    context.Context
	client *Client
	gvr    schema.GroupVersionResource
}

func (l *listerWatcher) List(opts metav1.ListOptions) (runtime.Object, error) {
	list, err := newList(l.gvr)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(l.ctx, l.client.timeout)
	defer cancel()
	err = l.client.rest.Get().AbsPath("/apis/"+l.gvr.Group+"/"+l.gvr.Version+"/"+l.gvr.Resource).VersionedParams(&opts, metav1.ParameterCodec).Do(requestCtx).Into(list)
	return list, err
}
func (l *listerWatcher) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return l.client.rest.Get().AbsPath("/apis/"+l.gvr.Group+"/"+l.gvr.Version+"/"+l.gvr.Resource).VersionedParams(&opts, metav1.ParameterCodec).Watch(l.ctx)
}

var _ cache.ListerWatcher = (*listerWatcher)(nil)

func newList(gvr schema.GroupVersionResource) (runtime.Object, error) {
	if gvr.Group != "ebs" || gvr.Version != "v1" {
		return nil, fmt.Errorf("unsupported resource %s", gvr)
	}
	switch gvr.Resource {
	case "configs":
		return &ebsv1.ConfigList{}, nil
	case "projects":
		return &ebsv1.ProjectList{}, nil
	case "snapshots":
		return &ebsv1.SnapshotList{}, nil
	case "builds":
		return &ebsv1.BuildList{}, nil
	case "buildinfos":
		return &ebsv1.BuildInfoList{}, nil
	case "rpmrepos":
		return &ebsv1.RpmRepoList{}, nil
	case "jobs":
		return &ebsv1.JobList{}, nil
	case "runners":
		return &ebsv1.RunnerList{}, nil
	default:
		return nil, fmt.Errorf("unsupported resource %s", gvr)
	}
}

func newObject(gvr schema.GroupVersionResource) (runtime.Object, error) {
	if gvr.Group != "ebs" || gvr.Version != "v1" {
		return nil, fmt.Errorf("unsupported resource %s", gvr)
	}
	switch gvr.Resource {
	case "configs":
		return &ebsv1.Config{}, nil
	case "projects":
		return &ebsv1.Project{}, nil
	case "snapshots":
		return &ebsv1.Snapshot{}, nil
	case "builds":
		return &ebsv1.Build{}, nil
	case "buildinfos":
		return &ebsv1.BuildInfo{}, nil
	case "rpmrepos":
		return &ebsv1.RpmRepo{}, nil
	case "jobs":
		return &ebsv1.Job{}, nil
	case "runners":
		return &ebsv1.Runner{}, nil
	default:
		return nil, fmt.Errorf("unsupported resource %s", gvr)
	}
}

func listPage(list runtime.Object) (source.ListPage, error) {
	page := source.ListPage{}
	switch value := list.(type) {
	case *ebsv1.ConfigList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	case *ebsv1.ProjectList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	case *ebsv1.SnapshotList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	case *ebsv1.BuildList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	case *ebsv1.BuildInfoList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	case *ebsv1.RpmRepoList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	case *ebsv1.JobList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	case *ebsv1.RunnerList:
		page.Continue, page.ResourceVersion = value.Continue, value.ResourceVersion
		for i := range value.Items {
			page.Items = append(page.Items, value.Items[i].DeepCopy())
		}
	default:
		return source.ListPage{}, fmt.Errorf("unsupported list type %T", list)
	}
	return page, nil
}

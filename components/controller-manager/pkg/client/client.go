package client

import (
	"context"
	"fmt"
	"time"

	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type Client struct {
	rest    *rest.RESTClient
	timeout time.Duration
}

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

func (c *Client) ListPage(ctx context.Context, gvr schema.GroupVersionResource, token string, limit int64) (source.ListPage, error) {
	list, err := newList(gvr)
	if err != nil {
		return source.ListPage{}, err
	}
	opts := metav1.ListOptions{Continue: token, Limit: limit}
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if err := c.rest.Get().AbsPath("/apis/"+gvr.Group+"/"+gvr.Version+"/"+gvr.Resource).VersionedParams(&opts, metav1.ParameterCodec).Do(requestCtx).Into(list); err != nil {
		return source.ListPage{}, err
	}
	return listPage(list)
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
	case "buildresources":
		return &ebsv1.BuildResourceList{}, nil
	case "jobs":
		return &ebsv1.JobList{}, nil
	case "runners":
		return &ebsv1.RunnerList{}, nil
	default:
		return nil, fmt.Errorf("unsupported resource %s", gvr)
	}
}

func listPage(list runtime.Object) (source.ListPage, error) {
	page := source.ListPage{}
	switch value := list.(type) {
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
	case *ebsv1.BuildResourceList:
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

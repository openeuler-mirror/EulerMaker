package client

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
)

type JobInterface interface {
	List(context.Context, metav1.ListOptions) (*ebsv1.JobList, error)
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
	Get(context.Context, string, string, metav1.GetOptions) (*ebsv1.Job, error)
	UpdateStatus(context.Context, string, string, *ebsv1.Job, metav1.UpdateOptions) (*ebsv1.Job, error)
}
type RunnerInterface interface {
	List(context.Context, metav1.ListOptions) (*ebsv1.RunnerList, error)
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
}
type Interface interface {
	Jobs() JobInterface
	Runners() RunnerInterface
}

type WriteOutcome string

const (
	WriteNotSent  WriteOutcome = "NotSent"
	WriteRejected WriteOutcome = "Rejected"
	WriteUnknown  WriteOutcome = "Unknown"
)

type WriteError struct {
	Outcome WriteOutcome
	Err     error
}

func (e *WriteError) Error() string { return fmt.Sprintf("write %s: %v", e.Outcome, e.Err) }
func (e *WriteError) Unwrap() error { return e.Err }

type Client struct {
	rest    *rest.RESTClient
	timeout time.Duration
	jobs    *jobs
	runners *runners
}

func New(config *rest.Config, timeout time.Duration) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("REST config is required")
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
		cfg.UserAgent = "eulermaker-scheduler/dev"
	}
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, err
	}
	c := &Client{rest: rc, timeout: timeout}
	c.jobs = &jobs{c: c}
	c.runners = &runners{c: c}
	return c, nil
}
func (c *Client) Jobs() JobInterface       { return c.jobs }
func (c *Client) Runners() RunnerInterface { return c.runners }
func segment(value string) string          { return url.PathEscape(value) }
func (c *Client) short(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

type jobs struct{ c *Client }

func (j *jobs) List(ctx context.Context, opts metav1.ListOptions) (*ebsv1.JobList, error) {
	if opts.Watch {
		return nil, fmt.Errorf("ListOptions.watch is not valid for List")
	}
	ctx, cancel := j.c.short(ctx)
	defer cancel()
	out := &ebsv1.JobList{}
	err := j.c.rest.Get().AbsPath("/apis/ebs/v1/jobs").VersionedParams(&opts, metav1.ParameterCodec).Do(ctx).Into(out)
	return out, err
}
func (j *jobs) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return j.c.rest.Get().AbsPath("/apis/ebs/v1/jobs").VersionedParams(&opts, metav1.ParameterCodec).Watch(ctx)
}
func (j *jobs) Get(ctx context.Context, project, name string, opts metav1.GetOptions) (*ebsv1.Job, error) {
	if project == "" || name == "" {
		return nil, fmt.Errorf("project and name are required")
	}
	ctx, cancel := j.c.short(ctx)
	defer cancel()
	out := &ebsv1.Job{}
	err := j.c.rest.Get().AbsPath("/apis/ebs/v1/projects/"+segment(project)+"/jobs/"+segment(name)).VersionedParams(&opts, metav1.ParameterCodec).Do(ctx).Into(out)
	return out, err
}
func (j *jobs) UpdateStatus(ctx context.Context, project, name string, job *ebsv1.Job, opts metav1.UpdateOptions) (*ebsv1.Job, error) {
	if project == "" || name == "" || job == nil || job.Namespace != project || job.Name != name || job.UID == "" || job.ResourceVersion == "" {
		return nil, &WriteError{Outcome: WriteNotSent, Err: fmt.Errorf("valid project, name, UID, and resourceVersion are required")}
	}
	ctx, cancel := j.c.short(ctx)
	defer cancel()
	var wrote atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { wrote.Store(true) }, WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }}
	ctx = httptrace.WithClientTrace(ctx, trace)
	out := &ebsv1.Job{}
	err := j.c.rest.Put().AbsPath("/apis/ebs/v1/projects/"+segment(project)+"/jobs/"+segment(name)+"/status").VersionedParams(&opts, metav1.ParameterCodec).Body(job).Do(ctx).Into(out)
	if err == nil {
		return out, nil
	}
	outcome := WriteUnknown
	var apiStatus interface{ Status() metav1.Status }
	if errors.As(err, &apiStatus) {
		outcome = WriteRejected
	} else if !wrote.Load() {
		outcome = WriteNotSent
	}
	return nil, &WriteError{Outcome: outcome, Err: err}
}

type runners struct{ c *Client }

func (r *runners) List(ctx context.Context, opts metav1.ListOptions) (*ebsv1.RunnerList, error) {
	if opts.Watch {
		return nil, fmt.Errorf("ListOptions.watch is not valid for List")
	}
	ctx, cancel := r.c.short(ctx)
	defer cancel()
	out := &ebsv1.RunnerList{}
	err := r.c.rest.Get().AbsPath("/apis/ebs/v1/runners").VersionedParams(&opts, metav1.ParameterCodec).Do(ctx).Into(out)
	return out, err
}
func (r *runners) Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	opts.Watch = true
	return r.c.rest.Get().AbsPath("/apis/ebs/v1/runners").VersionedParams(&opts, metav1.ParameterCodec).Watch(ctx)
}

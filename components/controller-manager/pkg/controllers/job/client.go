package job

import (
	"context"
	"fmt"

	clientpkg "controller-manager/pkg/client"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

type Client interface {
	GetJob(context.Context, string, string) (*ebsv1.Job, error)
	GetRunner(context.Context, string) (*ebsv1.Runner, error)
	UpdateJobStatus(context.Context, *ebsv1.Job) (*ebsv1.Job, error)
	DeleteJob(context.Context, string, string, clientpkg.DeletePreconditions) error
}

type apiClient struct{ client clientpkg.Interface }

func newAPIClient(value clientpkg.Interface) Client { return &apiClient{client: value} }

func (c *apiClient) GetJob(ctx context.Context, namespace, name string) (*ebsv1.Job, error) {
	obj, err := c.client.Get(ctx, source.JobsGVR, namespace, name)
	if err != nil {
		return nil, err
	}
	job, ok := obj.(*ebsv1.Job)
	if !ok || job == nil || job.Namespace != namespace || job.Name != name || job.UID == "" || job.ResourceVersion == "" {
		return nil, fmt.Errorf("unexpected Job response for %s/%s: %T", namespace, name, obj)
	}
	return job, nil
}

func (c *apiClient) GetRunner(ctx context.Context, name string) (*ebsv1.Runner, error) {
	obj, err := c.client.Get(ctx, source.RunnersGVR, "", name)
	if err != nil {
		return nil, err
	}
	runner, ok := obj.(*ebsv1.Runner)
	if !ok || runner == nil || runner.Name != name || runner.UID == "" || runner.ResourceVersion == "" {
		return nil, fmt.Errorf("unexpected Runner response for %s: %T", name, obj)
	}
	return runner, nil
}

func (c *apiClient) UpdateJobStatus(ctx context.Context, request *ebsv1.Job) (*ebsv1.Job, error) {
	if request == nil {
		return nil, &clientpkg.WriteError{Operation: "update-status", Resource: source.JobsGVR.GroupResource(), Outcome: clientpkg.WriteNotSent, Err: fmt.Errorf("nil Job request")}
	}
	obj, err := c.client.UpdateStatus(ctx, source.JobsGVR, request.Namespace, request)
	if err != nil {
		return nil, err
	}
	updated, ok := obj.(*ebsv1.Job)
	if !ok || updated == nil || updated.Namespace != request.Namespace || updated.Name != request.Name || updated.UID != request.UID || updated.ResourceVersion == "" ||
		updated.Status.Phase != ebsv1.JobFailed || updated.Status.Runner != request.Status.Runner || updated.Status.Stage != request.Status.Stage {
		return nil, writeUnknown("update-status", fmt.Errorf("unexpected Job status response: %T", obj))
	}
	return updated, nil
}

func (c *apiClient) DeleteJob(ctx context.Context, namespace, name string, preconditions clientpkg.DeletePreconditions) error {
	return c.client.Delete(ctx, source.JobsGVR, namespace, name, preconditions)
}

func writeUnknown(operation string, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: source.JobsGVR.GroupResource(), Outcome: clientpkg.WriteUnknown, Err: err}
}

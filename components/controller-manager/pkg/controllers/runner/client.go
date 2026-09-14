package runner

import (
	"context"
	"fmt"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

type Client interface {
	GetRunner(context.Context, string) (*ebsv1.Runner, error)
	UpdateRunnerStatus(context.Context, *ebsv1.Runner) (*ebsv1.Runner, error)
}

type apiClient struct{ client clientpkg.Interface }

func newAPIClient(value clientpkg.Interface) Client { return &apiClient{client: value} }

func (c *apiClient) GetRunner(ctx context.Context, name string) (*ebsv1.Runner, error) {
	obj, err := c.client.Get(ctx, source.RunnersGVR, "", name)
	if err != nil {
		return nil, err
	}
	runner, ok := obj.(*ebsv1.Runner)
	if !ok || runner == nil || runner.Name != name || runner.Namespace != "" || runner.UID == "" || runner.ResourceVersion == "" {
		return nil, fmt.Errorf("unexpected Runner response for %s: %T", name, obj)
	}
	return runner, nil
}

func (c *apiClient) UpdateRunnerStatus(ctx context.Context, request *ebsv1.Runner) (*ebsv1.Runner, error) {
	if request == nil {
		return nil, &clientpkg.WriteError{Operation: "update-status", Resource: source.RunnersGVR.GroupResource(), Outcome: clientpkg.WriteNotSent, Err: fmt.Errorf("nil Runner request")}
	}
	obj, err := c.client.UpdateStatus(ctx, source.RunnersGVR, "", request)
	if err != nil {
		return nil, err
	}
	updated, ok := obj.(*ebsv1.Runner)
	if !ok || updated == nil {
		return nil, runnerWriteUnknown(fmt.Errorf("unexpected Runner status response: %T", obj))
	}
	return updated, nil
}

func runnerWriteUnknown(err error) error {
	return &clientpkg.WriteError{Operation: "update-status", Resource: source.RunnersGVR.GroupResource(), Outcome: clientpkg.WriteUnknown, Err: err}
}

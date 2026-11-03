package job

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/serviceconfig"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// Client validates response identity and required fields; malformed write responses return WriteUnknown.
type Client interface {
	ListDeletingPage(context.Context, bool, string, int64) (source.ListPage, error)
	GetArchiveState(context.Context, string, string) (*ebsv1.JobArchiveState, error)
	FinalizeJob(context.Context, *ebsv1.Job, bool) error
	DeleteOldJob(context.Context, *ebsv1.Job) error
	DeleteJobData(context.Context, *ebsv1.Job) error
	ListHistoryPage(context.Context, ebsv1.JobPhase, string, int64) (source.ListPage, error)
	GetJob(context.Context, string, string) (*ebsv1.Job, error)
	GetRunner(context.Context, string) (*ebsv1.Runner, error)
	UpdateJobStatus(context.Context, *ebsv1.Job) (*ebsv1.Job, error)
	DeleteJob(context.Context, string, string, clientpkg.DeletePreconditions) error
	GetOldJob(context.Context, string, string) (*ebsv1.Job, error)
	ArchiveJob(context.Context, string, string, clientpkg.DeletePreconditions) (*ebsv1.Job, error)
}

type apiClient struct {
	client   *clientpkg.Client
	services serviceconfig.Provider
	http     *http.Client
}

type cleanupError struct {
	status     int
	retryAfter time.Duration
	body       string
}

func (e *cleanupError) Error() string {
	return fmt.Sprintf("Artifact Manager Job cleanup returned HTTP %d: %s", e.status, e.body)
}

func newAPIClient(value *clientpkg.Client, services serviceconfig.Provider, timeout time.Duration) Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &apiClient{client: value, services: services, http: &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *apiClient) ListDeletingPage(ctx context.Context, oldJob bool, token string, limit int64) (source.ListPage, error) {
	gvr := source.JobsGVR
	if oldJob {
		gvr = ebsv1.SchemeGroupVersion.WithResource("oldjobs")
	}
	return c.client.ListPage(ctx, gvr, metav1.ListOptions{FieldSelector: "metadata.deletionTimestamp!=null", Continue: token, Limit: limit})
}

func (c *apiClient) GetArchiveState(ctx context.Context, namespace, name string) (*ebsv1.JobArchiveState, error) {
	var state ebsv1.JobArchiveState
	err := c.client.GetSubresource(ctx, source.JobsGVR, namespace, name, "archive", &state)
	if err != nil {
		return nil, err
	}
	if state.UID == "" || (state.Action != "retain" && state.Action != "delete") ||
		(state.Phase != "Pending" && state.Phase != "Stored" && state.Phase != "Transferred") ||
		(state.Phase != "Pending" && state.Digest == "") {
		return nil, fmt.Errorf("invalid Job archive response")
	}
	return &state, nil
}

func (c *apiClient) FinalizeJob(ctx context.Context, job *ebsv1.Job, oldJob bool) error {
	gvr := source.JobsGVR
	if oldJob {
		gvr = ebsv1.SchemeGroupVersion.WithResource("oldjobs")
	}
	obj, err := c.client.PutSubresource(ctx, gvr, job.Namespace, job.Name, "finalizers", struct {
		Preconditions metav1.Preconditions `json:"preconditions"`
	}{metav1.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion}})
	if err != nil {
		return err
	}
	out, ok := obj.(*ebsv1.Job)
	finalizer := ebsv1.JobArchiveFinalizer
	if oldJob {
		finalizer = ebsv1.JobArtifactsCleanupFinalizer
	}
	if !ok || out.UID != job.UID || out.DeletionTimestamp == nil || slices.Contains(out.Finalizers, finalizer) {
		return writeUnknown("finalize", fmt.Errorf("unexpected finalizer response"))
	}
	return nil
}

func (c *apiClient) DeleteOldJob(ctx context.Context, job *ebsv1.Job) error {
	return c.client.Delete(ctx, ebsv1.SchemeGroupVersion.WithResource("oldjobs"), job.Namespace, job.Name,
		clientpkg.DeletePreconditions{UID: job.UID, ResourceVersion: job.ResourceVersion})
}

func (c *apiClient) DeleteJobData(ctx context.Context, job *ebsv1.Job) error {
	instance := job.Annotations[ebsv1.ProjectArtifactsAnnotation]
	if instance == "" {
		instance = "default"
	}
	address, err := serviceconfig.NewRound(c.services).ArtifactURL(ctx, instance)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(address, "/") + "/internal/v1/projects/" + url.PathEscape(job.Namespace) + "/jobs/" + url.PathEscape(job.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		delay := 5 * time.Second
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 && seconds <= 3600 {
			delay = time.Duration(seconds) * time.Second
		}
		return &cleanupError{status: resp.StatusCode, retryAfter: delay, body: string(body)}
	}
	return nil
}

func (c *apiClient) ListHistoryPage(ctx context.Context, phase ebsv1.JobPhase, token string, limit int64) (source.ListPage, error) {
	return c.client.ListPage(ctx, source.JobsGVR, metav1.ListOptions{
		FieldSelector: "status.phase=" + string(phase), Continue: token, Limit: limit,
	})
}

func (c *apiClient) GetJob(ctx context.Context, namespace, name string) (*ebsv1.Job, error) {
	return c.getJob(ctx, source.JobsGVR, namespace, name)
}

func (c *apiClient) GetOldJob(ctx context.Context, namespace, name string) (*ebsv1.Job, error) {
	return c.getJob(ctx, ebsv1.SchemeGroupVersion.WithResource("oldjobs"), namespace, name)
}

func (c *apiClient) getJob(ctx context.Context, resource schema.GroupVersionResource, namespace, name string) (*ebsv1.Job, error) {
	obj, err := c.client.Get(ctx, resource, namespace, name)
	if err != nil {
		return nil, err
	}
	job, ok := obj.(*ebsv1.Job)
	if !ok || job == nil || job.Namespace != namespace || job.Name != name || job.UID == "" || job.ResourceVersion == "" {
		return nil, fmt.Errorf("unexpected %s response for %s/%s: %T", resource.Resource, namespace, name, obj)
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

func (c *apiClient) ArchiveJob(ctx context.Context, namespace, name string, pre clientpkg.DeletePreconditions) (*ebsv1.Job, error) {
	return c.client.ArchiveJob(ctx, namespace, name, pre)
}

func writeUnknown(operation string, err error) error {
	return &clientpkg.WriteError{Operation: operation, Resource: source.JobsGVR.GroupResource(), Outcome: clientpkg.WriteUnknown, Err: err}
}

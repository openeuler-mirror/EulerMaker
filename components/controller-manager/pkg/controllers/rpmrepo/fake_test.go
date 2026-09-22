package rpmrepo

import (
	"context"
	"fmt"
	"strconv"

	clientpkg "controller-manager/pkg/clients/apiserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// FakeClient is an in-memory Client for unit tests. It records the request options and status writes so a test
// can assert selector pass-through and that no intermediate status was persisted.
type FakeClient struct {
	Builds     map[string]*ebsv1.Build
	BuildInfos map[string]*ebsv1.BuildInfo
	RpmRepos   map[string]*ebsv1.RpmRepo
	Jobs       map[string][]ebsv1.Job
	RpmRepoSet map[string][]ebsv1.RpmRepo

	GetBuildErr     error
	GetBuildInfoErr error
	GetRpmRepoErr   error
	ListJobsErr     error
	ListRpmReposErr error
	UpdateStatusErr error

	// UpdateStatusHook, when set, may reject or rewrite a status write to model conflicts and unknown outcomes.
	UpdateStatusHook func(request, current *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error)
	// GetBuildFunc, when set, replaces the preset Build lookup so a test can script per-call behaviour.
	GetBuildFunc func(ctx context.Context, project, name string) (*ebsv1.Build, error)

	JobListOptions     []metav1.ListOptions
	RpmRepoListOptions []metav1.ListOptions
	JobsQueries        []string
	RpmReposQueries    []string
	StatusWrites       []*ebsv1.RpmRepo
	GetRpmRepoCalls    int
	GetBuildCalls      int
	GetBuildInfoCalls  int
}

func NewFakeClient() *FakeClient {
	return &FakeClient{
		Builds:     map[string]*ebsv1.Build{},
		BuildInfos: map[string]*ebsv1.BuildInfo{},
		RpmRepos:   map[string]*ebsv1.RpmRepo{},
		Jobs:       map[string][]ebsv1.Job{},
		RpmRepoSet: map[string][]ebsv1.RpmRepo{},
	}
}

func key(project, name string) string { return project + "/" + name }

func (f *FakeClient) GetBuild(ctx context.Context, project, name string) (*ebsv1.Build, error) {
	f.GetBuildCalls++
	if f.GetBuildFunc != nil {
		return f.GetBuildFunc(ctx, project, name)
	}
	if f.GetBuildErr != nil {
		return nil, f.GetBuildErr
	}
	value, ok := f.Builds[key(project, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "builds"}, name)
	}
	return value.DeepCopy(), nil
}

func (f *FakeClient) GetBuildInfo(_ context.Context, project, name string) (*ebsv1.BuildInfo, error) {
	f.GetBuildInfoCalls++
	if f.GetBuildInfoErr != nil {
		return nil, f.GetBuildInfoErr
	}
	value, ok := f.BuildInfos[key(project, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "buildinfos"}, name)
	}
	return value.DeepCopy(), nil
}

func (f *FakeClient) GetRpmRepo(_ context.Context, project, name string) (*ebsv1.RpmRepo, error) {
	f.GetRpmRepoCalls++
	if f.GetRpmRepoErr != nil {
		return nil, f.GetRpmRepoErr
	}
	value, ok := f.RpmRepos[key(project, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "rpmrepos"}, name)
	}
	return value.DeepCopy(), nil
}

func (f *FakeClient) ListJobs(_ context.Context, project string, options metav1.ListOptions) ([]ebsv1.Job, error) {
	f.JobListOptions = append(f.JobListOptions, options)
	f.JobsQueries = append(f.JobsQueries, project)
	if f.ListJobsErr != nil {
		return nil, f.ListJobsErr
	}
	items := f.Jobs[project]
	result := make([]ebsv1.Job, 0, len(items))
	for _, item := range items {
		result = append(result, *item.DeepCopy())
	}
	return result, nil
}

func (f *FakeClient) ListRpmRepos(_ context.Context, project string, options metav1.ListOptions) ([]ebsv1.RpmRepo, error) {
	f.RpmRepoListOptions = append(f.RpmRepoListOptions, options)
	f.RpmReposQueries = append(f.RpmReposQueries, project)
	if f.ListRpmReposErr != nil {
		return nil, f.ListRpmReposErr
	}
	items := f.RpmRepoSet[project]
	result := make([]ebsv1.RpmRepo, 0, len(items))
	for _, item := range items {
		result = append(result, *item.DeepCopy())
	}
	return result, nil
}

func (f *FakeClient) UpdateRpmRepoStatus(_ context.Context, obj *ebsv1.RpmRepo) (*ebsv1.RpmRepo, error) {
	if obj == nil {
		return nil, notSentWrite("update-status", source.RpmReposGVR, fmt.Errorf("RpmRepo request is required"))
	}
	current, ok := f.RpmRepos[key(obj.Namespace, obj.Name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "ebs", Resource: "rpmrepos"}, obj.Name)
	}
	if f.UpdateStatusHook != nil {
		return f.UpdateStatusHook(obj, current.DeepCopy())
	}
	if obj.ResourceVersion != current.ResourceVersion {
		return nil, &clientpkg.WriteError{Operation: "update-status", Resource: source.RpmReposGVR.GroupResource(), Outcome: clientpkg.WriteRejected, StatusCode: 409,
			Err: fmt.Errorf("RpmRepo %s/%s resourceVersion %q is stale", obj.Namespace, obj.Name, obj.ResourceVersion)}
	}
	if f.UpdateStatusErr != nil {
		return nil, f.UpdateStatusErr
	}
	updated := current.DeepCopy()
	updated.Status = obj.DeepCopy().Status
	next, _ := strconv.Atoi(current.ResourceVersion)
	updated.ResourceVersion = strconv.Itoa(next + 1)
	f.RpmRepos[key(obj.Namespace, obj.Name)] = updated
	f.StatusWrites = append(f.StatusWrites, updated.DeepCopy())
	return updated.DeepCopy(), nil
}

// Reset clears the recorded calls while keeping the preset objects and hooks, so a test can compare two
// rounds of the same reconcile.
func (f *FakeClient) Reset() {
	f.JobListOptions = nil
	f.RpmRepoListOptions = nil
	f.JobsQueries = nil
	f.RpmReposQueries = nil
	f.StatusWrites = nil
	f.GetRpmRepoCalls = 0
	f.GetBuildCalls = 0
	f.GetBuildInfoCalls = 0
}

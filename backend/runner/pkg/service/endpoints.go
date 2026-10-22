package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ebs-runner/pkg/artifact"
	"ebs-runner/pkg/gateway"
)

// Routes pins each active Job's destination while new Jobs use refreshed addresses.
type Routes struct {
	mu        sync.Mutex
	instances map[string]gateway.ServiceEndpoint
	gitServer gateway.ServiceEndpoint
	jobs      map[string]*jobRoute
	changed   chan struct{}
	mode      string
	tokens    gateway.TokenSource
	http      *http.Client
}

type jobRoute struct {
	client      *artifact.Client
	artifactURL string
	gitURL      string
}

func NewRoutes(mode string, tokens gateway.TokenSource, client *http.Client) *Routes {
	return &Routes{mode: mode, tokens: tokens, http: client, jobs: make(map[string]*jobRoute),
		changed: make(chan struct{})}
}

func (r *Routes) Update(endpoints gateway.ServiceEndpoints) error {
	if len(endpoints.ArtifactManagers) == 0 {
		return fmt.Errorf("artifact endpoint list is empty")
	}
	r.mu.Lock()
	r.instances = make(map[string]gateway.ServiceEndpoint, len(endpoints.ArtifactManagers))
	for name, entry := range endpoints.ArtifactManagers {
		r.instances[name] = entry
	}
	r.gitServer = endpoints.GitServer
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	return nil
}

func (r *Routes) Pin(ctx context.Context, job gateway.JobResource) error {
	key := job.Metadata.Namespace + "/" + job.Metadata.Name
	instance := artifactInstance(job)
	for {
		r.mu.Lock()
		if r.jobs[key] != nil {
			r.mu.Unlock()
			return nil
		}
		entry, exists := r.instances[instance]
		address := r.address(entry)
		if exists && address != "" {
			client, err := artifact.NewClient(address, r.tokens, r.http)
			if err == nil {
				r.jobs[key] = &jobRoute{client: client, artifactURL: address, gitURL: r.address(r.gitServer)}
			}
			r.mu.Unlock()
			return err
		}
		changed := r.changed
		r.mu.Unlock()
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-changed:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (r *Routes) Release(job gateway.JobResource) {
	r.mu.Lock()
	delete(r.jobs, job.Metadata.Namespace+"/"+job.Metadata.Name)
	r.mu.Unlock()
}

func (r *Routes) client(project, job string) (*artifact.Client, error) {
	r.mu.Lock()
	client := r.jobs[project+"/"+job]
	r.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("artifact destination for %s/%s is not pinned", project, job)
	}
	return client.client, nil
}

func (r *Routes) address(endpoint gateway.ServiceEndpoint) string {
	if r.mode == "external" {
		return endpoint.RunnerExternalURL
	}
	return endpoint.RunnerInternalURL
}

func (r *Routes) LogStatus(ctx context.Context, project, job string) (artifact.LogStatus, error) {
	client, err := r.client(project, job)
	if err != nil {
		return artifact.LogStatus{}, err
	}
	return client.LogStatus(ctx, project, job)
}

func (r *Routes) AppendLog(ctx context.Context, project, job string, sequence int64, sum string, data []byte) (artifact.AppendLogResult, error) {
	client, err := r.client(project, job)
	if err != nil {
		return artifact.AppendLogResult{}, err
	}
	return client.AppendLog(ctx, project, job, sequence, sum, data)
}

func (r *Routes) CompleteLog(ctx context.Context, project, job, key string, input artifact.CompleteLogInput) (artifact.CompletedLog, error) {
	client, err := r.client(project, job)
	if err != nil {
		return artifact.CompletedLog{}, err
	}
	return client.CompleteLog(ctx, project, job, key, input)
}

func (r *Routes) UploadArtifact(ctx context.Context, project, job, key, path string, input artifact.UploadArtifactInput) (artifact.ArtifactRecord, error) {
	client, err := r.client(project, job)
	if err != nil {
		return artifact.ArtifactRecord{}, err
	}
	return client.UploadArtifact(ctx, project, job, key, path, input)
}

func (r *Routes) CompleteManifest(ctx context.Context, project, job string, input artifact.CompleteManifestInput) (artifact.CompletedManifest, error) {
	client, err := r.client(project, job)
	if err != nil {
		return artifact.CompletedManifest{}, err
	}
	return client.CompleteManifest(ctx, project, job, input)
}

func (r *Routes) GetManifest(ctx context.Context, project, job string) (artifact.CompletedManifest, error) {
	client, err := r.client(project, job)
	if err != nil {
		return artifact.CompletedManifest{}, err
	}
	return client.GetManifest(ctx, project, job)
}

func artifactInstance(job gateway.JobResource) string {
	instance := strings.TrimSpace(job.Metadata.Annotations["ebs.io/artifacts"])
	if instance == "" {
		return "default"
	}
	return instance
}

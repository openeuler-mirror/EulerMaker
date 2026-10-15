package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type ArtifactEndpoints struct {
	Instances map[string]ArtifactEndpoint `json:"instances"`
}

type ArtifactEndpoint struct {
	RunnerInternalURL string `json:"runnerInternalURL"`
	RunnerExternalURL string `json:"runnerExternalURL"`
}

// artifactRoutes pins a destination for each active Job while allowing newly started Jobs to use refreshed addresses.
type artifactRoutes struct {
	mu        sync.Mutex
	instances map[string]ArtifactEndpoint
	jobs      map[string]*ArtifactClient
	changed   chan struct{}
	mode      string
	tokens    TokenSource
	http      *http.Client
}

func newArtifactRoutes(mode string, tokens TokenSource, client *http.Client) *artifactRoutes {
	return &artifactRoutes{mode: mode, tokens: tokens, http: client, jobs: make(map[string]*ArtifactClient),
		changed: make(chan struct{})}
}

func validEndpointURL(raw string, external bool) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		((external && u.Scheme == "https") || (!external && (u.Scheme == "http" || u.Scheme == "https")))
}

func (r *artifactRoutes) update(endpoints ArtifactEndpoints) error {
	if len(endpoints.Instances) == 0 {
		return fmt.Errorf("artifact endpoint list is empty")
	}
	for name, entry := range endpoints.Instances {
		if name == "" || !validEndpointURL(entry.RunnerInternalURL, false) ||
			!validEndpointURL(entry.RunnerExternalURL, true) {
			return fmt.Errorf("invalid artifact endpoint for instance %q", name)
		}
	}
	r.mu.Lock()
	r.instances = make(map[string]ArtifactEndpoint, len(endpoints.Instances))
	for name, entry := range endpoints.Instances {
		r.instances[name] = entry
	}
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
	return nil
}

func (r *artifactRoutes) pin(ctx context.Context, job JobResource) error {
	key := job.Metadata.Namespace + "/" + job.Metadata.Name
	instance := artifactInstance(job)
	for {
		r.mu.Lock()
		if r.jobs[key] != nil {
			r.mu.Unlock()
			return nil
		}
		entry, exists := r.instances[instance]
		address := entry.RunnerInternalURL
		if r.mode == "external" {
			address = entry.RunnerExternalURL
		}
		if exists && address != "" {
			client, err := NewArtifactClient(address, r.tokens, r.http)
			if err == nil {
				r.jobs[key] = client
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

func (r *artifactRoutes) release(job JobResource) {
	r.mu.Lock()
	delete(r.jobs, job.Metadata.Namespace+"/"+job.Metadata.Name)
	r.mu.Unlock()
}

func (r *artifactRoutes) client(project, job string) (*ArtifactClient, error) {
	r.mu.Lock()
	client := r.jobs[project+"/"+job]
	r.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("artifact destination for %s/%s is not pinned", project, job)
	}
	return client, nil
}

func (r *artifactRoutes) LogStatus(ctx context.Context, project, job string) (LogStatus, error) {
	client, err := r.client(project, job)
	if err != nil {
		return LogStatus{}, err
	}
	return client.LogStatus(ctx, project, job)
}

func (r *artifactRoutes) AppendLog(ctx context.Context, project, job string, sequence int64, sum string, data []byte) (AppendLogResult, error) {
	client, err := r.client(project, job)
	if err != nil {
		return AppendLogResult{}, err
	}
	return client.AppendLog(ctx, project, job, sequence, sum, data)
}

func (r *artifactRoutes) CompleteLog(ctx context.Context, project, job, key string, input CompleteLogInput) (CompletedLog, error) {
	client, err := r.client(project, job)
	if err != nil {
		return CompletedLog{}, err
	}
	return client.CompleteLog(ctx, project, job, key, input)
}

func (r *artifactRoutes) UploadArtifact(ctx context.Context, project, job, key, path string, input UploadArtifactInput) (ArtifactRecord, error) {
	client, err := r.client(project, job)
	if err != nil {
		return ArtifactRecord{}, err
	}
	return client.UploadArtifact(ctx, project, job, key, path, input)
}

func (r *artifactRoutes) CompleteManifest(ctx context.Context, project, job string, input CompleteManifestInput) (CompletedManifest, error) {
	client, err := r.client(project, job)
	if err != nil {
		return CompletedManifest{}, err
	}
	return client.CompleteManifest(ctx, project, job, input)
}

func (r *artifactRoutes) GetManifest(ctx context.Context, project, job string) (CompletedManifest, error) {
	client, err := r.client(project, job)
	if err != nil {
		return CompletedManifest{}, err
	}
	return client.GetManifest(ctx, project, job)
}

func artifactInstance(job JobResource) string {
	instance := strings.TrimSpace(job.Metadata.Annotations["ebs.io/artifacts"])
	if instance == "" {
		return "default"
	}
	return instance
}

package artifact

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ebs-runner/pkg/gateway"
)

// Routes pins each active Job's destination while new Jobs use refreshed addresses.
type Routes struct {
	mu        sync.Mutex
	instances map[string]gateway.ArtifactEndpoint
	jobs      map[string]*Client
	changed   chan struct{}
	mode      string
	tokens    gateway.TokenSource
	http      *http.Client
}

func NewRoutes(mode string, tokens gateway.TokenSource, client *http.Client) *Routes {
	return &Routes{mode: mode, tokens: tokens, http: client, jobs: make(map[string]*Client),
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

func (r *Routes) Update(endpoints gateway.ArtifactEndpoints) error {
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
	r.instances = make(map[string]gateway.ArtifactEndpoint, len(endpoints.Instances))
	for name, entry := range endpoints.Instances {
		r.instances[name] = entry
	}
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
		address := entry.RunnerInternalURL
		if r.mode == "external" {
			address = entry.RunnerExternalURL
		}
		if exists && address != "" {
			client, err := NewClient(address, r.tokens, r.http)
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

func (r *Routes) Release(job gateway.JobResource) {
	r.mu.Lock()
	delete(r.jobs, job.Metadata.Namespace+"/"+job.Metadata.Name)
	r.mu.Unlock()
}

func (r *Routes) client(project, job string) (*Client, error) {
	r.mu.Lock()
	client := r.jobs[project+"/"+job]
	r.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("artifact destination for %s/%s is not pinned", project, job)
	}
	return client, nil
}

func (r *Routes) LogStatus(ctx context.Context, project, job string) (LogStatus, error) {
	client, err := r.client(project, job)
	if err != nil {
		return LogStatus{}, err
	}
	return client.LogStatus(ctx, project, job)
}

func (r *Routes) AppendLog(ctx context.Context, project, job string, sequence int64, sum string, data []byte) (AppendLogResult, error) {
	client, err := r.client(project, job)
	if err != nil {
		return AppendLogResult{}, err
	}
	return client.AppendLog(ctx, project, job, sequence, sum, data)
}

func (r *Routes) CompleteLog(ctx context.Context, project, job, key string, input CompleteLogInput) (CompletedLog, error) {
	client, err := r.client(project, job)
	if err != nil {
		return CompletedLog{}, err
	}
	return client.CompleteLog(ctx, project, job, key, input)
}

func (r *Routes) UploadArtifact(ctx context.Context, project, job, key, path string, input UploadArtifactInput) (ArtifactRecord, error) {
	client, err := r.client(project, job)
	if err != nil {
		return ArtifactRecord{}, err
	}
	return client.UploadArtifact(ctx, project, job, key, path, input)
}

func (r *Routes) CompleteManifest(ctx context.Context, project, job string, input CompleteManifestInput) (CompletedManifest, error) {
	client, err := r.client(project, job)
	if err != nil {
		return CompletedManifest{}, err
	}
	return client.CompleteManifest(ctx, project, job, input)
}

func (r *Routes) GetManifest(ctx context.Context, project, job string) (CompletedManifest, error) {
	client, err := r.client(project, job)
	if err != nil {
		return CompletedManifest{}, err
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

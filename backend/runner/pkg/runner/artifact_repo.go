package runner

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// resolvePayload replaces logical repository URLs only in the Job's repo array.
func (r *artifactRoutes) resolvePayload(job JobResource) (string, error) {
	if !strings.Contains(job.Spec.Payload, "artifact:") {
		return job.Spec.Payload, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job.Spec.Payload), &payload); err != nil {
		return "", fmt.Errorf("decode Job payload: %w", err)
	}
	rawRepos, exists := payload["repo"]
	if !exists {
		return job.Spec.Payload, nil
	}
	var repos []map[string]json.RawMessage
	if err := json.Unmarshal(rawRepos, &repos); err != nil {
		return "", fmt.Errorf("decode Job payload repo: %w", err)
	}

	r.mu.Lock()
	addresses := make(map[string]string, len(r.instances))
	for name, entry := range r.instances {
		address := entry.RunnerInternalURL
		if r.mode == "external" {
			address = entry.RunnerExternalURL
		}
		addresses[name] = address
	}
	if pinned := r.jobs[job.Metadata.Namespace+"/"+job.Metadata.Name]; pinned != nil {
		addresses[artifactInstance(job)] = pinned.baseURL.String()
	}
	r.mu.Unlock()

	changed := false
	for i, repo := range repos {
		var reference string
		if err := json.Unmarshal(repo["url"], &reference); err != nil {
			return "", fmt.Errorf("decode Job payload repo[%d].url: %w", i, err)
		}
		if !strings.HasPrefix(reference, "artifact:") {
			continue
		}
		resolved, err := resolveArtifactRepository(reference, addresses)
		if err != nil {
			return "", fmt.Errorf("resolve Job payload repo[%d].url: %w", i, err)
		}
		repo["url"], err = json.Marshal(resolved)
		if err != nil {
			return "", err
		}
		changed = true
	}
	if !changed {
		return job.Spec.Payload, nil
	}
	encodedRepos, err := json.Marshal(repos)
	if err != nil {
		return "", fmt.Errorf("encode Job payload repo: %w", err)
	}
	payload["repo"] = encodedRepos
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode Job payload: %w", err)
	}
	return string(encoded), nil
}

func resolveArtifactRepository(reference string, addresses map[string]string) (string, error) {
	u, err := url.Parse(reference)
	if err != nil || u.Scheme != "artifact" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" ||
		!strings.HasPrefix(u.Path, "/repositories/") || strings.TrimSuffix(u.Path, "/") != path.Clean(u.Path) {
		return "", fmt.Errorf("invalid artifact repository URL %q", reference)
	}
	instance := u.Host
	if instance == "" {
		instance = "default"
	}
	address := addresses[instance]
	if address == "" {
		return "", fmt.Errorf("artifact instance %q has no Runner address", instance)
	}
	base, err := url.Parse(address)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", fmt.Errorf("artifact instance %q has an invalid Runner address", instance)
	}
	resolved := *base
	resolved.Path = singleJoiningSlash(base.Path, u.Path)
	resolved.RawPath = singleJoiningSlash(base.EscapedPath(), u.EscapedPath())
	return resolved.String(), nil
}

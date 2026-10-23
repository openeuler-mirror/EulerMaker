package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"ebs-runner/pkg/gateway"
)

// ResolvePayload resolves repository and clone URLs without changing the Job's stored payload.
func (r *Routes) ResolvePayload(job gateway.JobResource) (string, error) {
	if strings.TrimSpace(job.Spec.Payload) == "" {
		return job.Spec.Payload, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(job.Spec.Payload), &payload); err != nil {
		return "", fmt.Errorf("decode Job payload: %w", err)
	}
	rawRepos, exists := payload["repo"]
	var repos []map[string]json.RawMessage
	if exists {
		if err := json.Unmarshal(rawRepos, &repos); err != nil {
			return "", fmt.Errorf("decode Job payload repo: %w", err)
		}
	}

	r.mu.Lock()
	addresses := make(map[string]string, len(r.instances))
	for name, entry := range r.instances {
		addresses[name] = r.address(entry)
	}
	gitAddress := r.address(r.gitServer)
	if pinned := r.jobs[job.Metadata.Namespace+"/"+job.Metadata.Name]; pinned != nil {
		addresses[artifactInstance(job)] = pinned.artifactURL
		gitAddress = pinned.gitURL
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
	if changed {
		encodedRepos, err := json.Marshal(repos)
		if err != nil {
			return "", fmt.Errorf("encode Job payload repo: %w", err)
		}
		payload["repo"] = encodedRepos
	}
	if rawClone, ok := payload["clone_url"]; ok {
		var reference string
		if err := json.Unmarshal(rawClone, &reference); err != nil {
			return "", fmt.Errorf("decode Job payload clone_url: %w", err)
		}
		resolved := reference
		if r.mode == "external" {
			resolved = ""
			if err := json.Unmarshal(payload["origin_url"], &resolved); err != nil {
				return "", fmt.Errorf("decode Job payload origin_url for external clone: %w", err)
			}
			if strings.TrimSpace(resolved) == "" {
				return "", fmt.Errorf("Job payload origin_url is required for external clone")
			}
		} else if strings.HasPrefix(reference, "git-server:") {
			var err error
			resolved, err = resolveGitClone(reference, gitAddress)
			if err != nil {
				return "", fmt.Errorf("resolve Job payload clone_url: %w", err)
			}
		}
		if resolved != reference {
			payload["clone_url"], _ = json.Marshal(resolved)
			changed = true
		}
	}
	if !changed {
		return job.Spec.Payload, nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode Job payload: %w", err)
	}
	return string(encoded), nil
}

func resolveGitClone(reference, address string) (string, error) {
	u, err := url.Parse(reference)
	if err != nil || !strings.HasPrefix(reference, "git-server:///git/") || u.Host != "" || u.User != nil ||
		u.Opaque != "" || strings.ContainsAny(reference, "?#\\ \t\r\n") ||
		!strings.HasPrefix(u.Path, "/git/") || len(u.Path) <= len("/git/") ||
		u.Path != path.Clean(u.Path) || strings.ContainsAny(u.Path, "\\\x00\r\n") {
		return "", fmt.Errorf("invalid Git Server clone URL %q", reference)
	}
	if address == "" {
		return "", fmt.Errorf("Git Server has no Runner address for the selected access mode")
	}
	base, err := url.Parse(address)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery || strings.Contains(address, "#") {
		return "", fmt.Errorf("Git Server has an invalid Runner address")
	}
	// Preserve escapes from both the proxy prefix and repository key exactly once.
	escapedBase := base.EscapedPath()
	base.Path = singleJoiningSlash(base.Path, u.Path)
	base.RawPath = singleJoiningSlash(escapedBase, u.EscapedPath())
	return base.String(), nil
}

func singleJoiningSlash(a, b string) string {
	return strings.TrimRight(a, "/") + "/" + strings.TrimLeft(b, "/")
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

package artifacturl

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Reference records an Artifact Manager repository path without binding it to
// the address used by a particular controller or Job container.
func Reference(repositoryPath string) (string, error) {
	u, err := repositoryPathURL(repositoryPath)
	if err != nil {
		return "", err
	}
	return "artifact://" + u.EscapedPath(), nil
}

// Resolve turns an artifact reference into an HTTP URL reachable by both the
// controller-manager and Job containers. Existing absolute HTTP URLs remain
// usable, and bare repository paths from older RpmRepo objects are accepted.
func Resolve(address, reference string) (string, error) {
	if reference == "" {
		return "", nil
	}
	u, err := url.Parse(reference)
	if err != nil {
		return "", fmt.Errorf("parse repository URL: %w", err)
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		if u.Host == "" {
			return "", fmt.Errorf("repository HTTP URL has no host")
		}
		return reference, nil
	}
	if u.Scheme != "" && u.Scheme != "artifact" {
		return "", fmt.Errorf("unsupported repository URL scheme %q", u.Scheme)
	}
	if u.Scheme == "artifact" && (u.Host != "" || u.Opaque != "") {
		return "", fmt.Errorf("artifact repository URL must not have a host")
	}
	if _, err := repositoryPathURL(u.EscapedPath()); err != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid artifact repository path %q", reference)
	}
	base, err := url.Parse(address)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", fmt.Errorf("artifact-manager-addr must be an absolute HTTP URL")
	}
	return base.ResolveReference(&url.URL{Path: u.Path, RawPath: u.RawPath}).String(), nil
}

func repositoryPathURL(value string) (*url.URL, error) {
	u, err := url.ParseRequestURI(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || !strings.HasPrefix(u.Path, "/repositories/") ||
		strings.TrimSuffix(u.Path, "/") != path.Clean(u.Path) {
		return nil, fmt.Errorf("invalid Artifact Manager repository path %q", value)
	}
	return u, nil
}

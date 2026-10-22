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
	return ReferenceForInstance("", repositoryPath)
}

// ReferenceForInstance records a repository path with its owning Artifact Manager instance.
func ReferenceForInstance(instance, repositoryPath string) (string, error) {
	u, err := repositoryPathURL(repositoryPath)
	if err != nil {
		return "", err
	}
	if instance != "" {
		if strings.ContainsAny(instance, "/:@?#") || instance != strings.ToLower(instance) {
			return "", fmt.Errorf("invalid Artifact Manager instance %q", instance)
		}
		return "artifact://" + instance + u.EscapedPath(), nil
	}
	return "artifact://" + u.EscapedPath(), nil
}

// Resolve maps the instance in a logical repository reference to an internal HTTP URL.
// Hostless references and bare repository paths use the default instance.
func Resolve(resolveAddress func(instance string) (string, error), reference string) (string, error) {
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
	if u.Scheme == "artifact" && (u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "") {
		return "", fmt.Errorf("invalid artifact repository reference %q", reference)
	}
	if _, err := repositoryPathURL(u.EscapedPath()); err != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid artifact repository path %q", reference)
	}
	instance := u.Host
	if instance == "" {
		instance = "default"
	}
	if resolveAddress == nil {
		return "", fmt.Errorf("artifact manager address resolver is required")
	}
	address, err := resolveAddress(instance)
	if err != nil {
		return "", err
	}
	base, err := url.Parse(address)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", fmt.Errorf("artifact manager instance %q must have an absolute HTTP URL", instance)
	}
	base.RawPath = strings.TrimRight(base.EscapedPath(), "/") + u.EscapedPath()
	base.Path = strings.TrimRight(base.Path, "/") + u.Path
	return base.String(), nil
}

func repositoryPathURL(value string) (*url.URL, error) {
	u, err := url.ParseRequestURI(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.RawQuery != "" || !strings.HasPrefix(u.Path, "/repositories/") ||
		strings.TrimSuffix(u.Path, "/") != path.Clean(u.Path) {
		return nil, fmt.Errorf("invalid Artifact Manager repository path %q", value)
	}
	return u, nil
}

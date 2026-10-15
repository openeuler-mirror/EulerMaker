package apiserver

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

type artifactManagerConfigContent struct {
	Instances map[string]struct {
		InternalURL       string `json:"internalURL"`
		RunnerInternalURL string `json:"runnerInternalURL,omitempty"`
		RunnerExternalURL string `json:"runnerExternalURL,omitempty"`
	} `json:"instances"`
}

// ParseArtifactManagerEndpoints returns the control-plane URL of each configured instance.
func ParseArtifactManagerEndpoints(content string) (map[string]string, error) {
	var config artifactManagerConfigContent
	if err := yaml.UnmarshalStrict([]byte(content), &config); err != nil {
		return nil, fmt.Errorf("invalid artifact-manager-conf Config content: %w", err)
	}
	if len(config.Instances) == 0 {
		return nil, fmt.Errorf("artifact-manager-conf Config has no instances")
	}
	endpoints := make(map[string]string, len(config.Instances))
	for name, instance := range config.Instances {
		if name == "" || instance.InternalURL == "" {
			return nil, fmt.Errorf("invalid artifact-manager-conf instance %q", name)
		}
		endpoints[name] = instance.InternalURL
	}
	return endpoints, nil
}

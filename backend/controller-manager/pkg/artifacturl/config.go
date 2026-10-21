package artifacturl

import (
	"fmt"

	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
)

// ParseArtifactManagerEndpoints returns the control-plane URL of each configured instance.
func ParseArtifactManagerEndpoints(content string) (map[string]string, error) {
	var config ebsv1.ArtifactManagerContent
	if err := yaml.UnmarshalStrict([]byte(content), &config); err != nil {
		return nil, fmt.Errorf("invalid artifact-manager-conf Config content: %w", err)
	}
	endpoints := make(map[string]string, len(config.Instances))
	for name, instance := range config.Instances {
		endpoints[name] = instance.InternalURL
	}
	return endpoints, nil
}

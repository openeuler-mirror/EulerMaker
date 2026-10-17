package apiserver

import (
	"context"
	"fmt"

	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
)

// GetBuildTargetContent returns a snapshot for one Job creation batch. Callers must not
// refetch it per Job, or use it to mutate an already created Job.
func (c *Client) GetBuildTargetContent(ctx context.Context) (*ebsv1.BuildTargetContent, error) {
	obj, err := c.Get(ctx, ebsv1.SchemeGroupVersion.WithResource("configs"), "", ebsv1.BuildTargetConfigName)
	if err != nil {
		return nil, err
	}
	config, ok := obj.(*ebsv1.Config)
	if !ok || config == nil || config.Name != ebsv1.BuildTargetConfigName {
		return nil, fmt.Errorf("expected build-target Config, got %T", obj)
	}
	var content ebsv1.BuildTargetContent
	if err := yaml.UnmarshalStrict([]byte(config.Spec.Content), &content); err != nil {
		return nil, fmt.Errorf("decode build-target Config: %w", err)
	}
	return &content, nil
}

func BuildImage(conf *ebsv1.BuildTargetContent, target ebsv1.BuildTarget) (string, error) {
	if conf == nil {
		return "", fmt.Errorf("build-target Config is unavailable")
	}
	image := conf.Targets[target.Os].Arches[target.Arch].Image
	if image == "" {
		return "", fmt.Errorf("build-target Config has no image for %s/%s", target.Os, target.Arch)
	}
	return image, nil
}

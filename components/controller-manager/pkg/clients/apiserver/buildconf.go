package apiserver

import (
	"context"
	"fmt"

	ebsv1 "ebs-api/ebs/v1"
)

// GetBuildConf returns a snapshot for one Job creation batch. Callers must not
// refetch it per Job, or use it to mutate an already created Job.
func (c *Client) GetBuildConf(ctx context.Context) (*ebsv1.BuildConf, error) {
	obj, err := c.Get(ctx, ebsv1.SchemeGroupVersion.WithResource("buildconfs"), "", "default")
	if err != nil {
		return nil, err
	}
	conf, ok := obj.(*ebsv1.BuildConf)
	if !ok {
		return nil, fmt.Errorf("expected BuildConf, got %T", obj)
	}
	return conf, nil
}

func BuildImage(conf *ebsv1.BuildConf, target ebsv1.BuildTarget) (string, error) {
	if conf == nil {
		return "", fmt.Errorf("BuildConf is unavailable")
	}
	image := conf.Spec.Targets[target.Os].Arches[target.Arch].Image
	if image == "" {
		return "", fmt.Errorf("BuildConf has no image for %s/%s", target.Os, target.Arch)
	}
	return image, nil
}

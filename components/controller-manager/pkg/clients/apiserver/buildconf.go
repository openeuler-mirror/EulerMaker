package apiserver

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	ebsv1 "ebs-api/ebs/v1"
	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

var configArchPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// GetBuildConf returns a snapshot for one Job creation batch. Callers must not
// refetch it per Job, or use it to mutate an already created Job.
func (c *Client) GetBuildConf(ctx context.Context) (*ebsv1.BuildConf, error) {
	obj, err := c.Get(ctx, ebsv1.SchemeGroupVersion.WithResource("configs"), "", ebsv1.BuildTargetConfigName)
	if err != nil {
		return nil, err
	}
	config, ok := obj.(*ebsv1.Config)
	if !ok || config == nil || config.Name != ebsv1.BuildTargetConfigName {
		return nil, fmt.Errorf("expected build-target Config, got %T", obj)
	}
	var content ebsv1.BuildConfSpec
	if err := yaml.UnmarshalStrict([]byte(config.Spec.Content), &content); err != nil {
		return nil, fmt.Errorf("decode build-target Config: %w", err)
	}
	if content.Targets == nil {
		return nil, fmt.Errorf("build-target Config requires targets map")
	}
	for os, target := range content.Targets {
		if os == "" || strings.TrimSpace(os) != os || len(validation.IsValidLabelValue(os)) != 0 || len(target.Arches) == 0 {
			return nil, fmt.Errorf("invalid build-target OS %q", os)
		}
		for arch, entry := range target.Arches {
			_, imageErr := reference.ParseNormalizedNamed(entry.Image)
			if !configArchPattern.MatchString(arch) || imageErr != nil || strings.ContainsAny(entry.Image, " \t\r\n") {
				return nil, fmt.Errorf("invalid build-target %s/%s image", os, arch)
			}
		}
	}
	// Keep the existing controller-internal snapshot shape while the persisted
	// resource is Config/build-target. The real Config identity was checked above.
	metadata := config.ObjectMeta
	metadata.Name = "default"
	return &ebsv1.BuildConf{ObjectMeta: metadata, Spec: content}, nil
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

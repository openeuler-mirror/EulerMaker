package server

import (
	"context"
	_ "embed"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
)

const defaultBuildResourceConfigName = "default"

//go:embed default-build-resource-config.yaml
var defaultBuildResourceConfigTemplate []byte

type defaultBuildResourceConfigStorage interface {
	rest.Getter
	rest.Creater
}

func ensureDefaultBuildResourceConfig(ctx context.Context, storage defaultBuildResourceConfigStorage) error {
	ctx = genericapirequest.WithNamespace(ctx, "")
	_, err := storage.Get(ctx, defaultBuildResourceConfigName, &metav1.GetOptions{})
	switch {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get default BuildResourceConfig: %w", err)
	}

	obj := new(ebsv1.BuildResourceConfig)
	if err := yaml.UnmarshalStrict(defaultBuildResourceConfigTemplate, obj); err != nil {
		return fmt.Errorf("decode embedded default BuildResourceConfig: %w", err)
	}
	_, err = storage.Create(ctx, obj, nil, &metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create default BuildResourceConfig: %w", err)
	}
	return nil
}

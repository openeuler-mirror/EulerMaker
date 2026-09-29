package server

import (
	"context"
	_ "embed"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
)

//go:embed default-build-target.yaml
var defaultBuildTargetTemplate []byte

//go:embed default-build-resource.yaml
var defaultBuildResourceTemplate []byte

type bootstrapStorage interface {
	rest.Getter
	rest.Creater
}

func ensureDefaultConfigs(ctx context.Context, storage bootstrapStorage) error {
	var target ebsv1.BuildTargetContent
	if err := yaml.UnmarshalStrict(defaultBuildTargetTemplate, &target); err != nil {
		return err
	}
	var resources ebsv1.BuildResourceContent
	if err := yaml.UnmarshalStrict(defaultBuildResourceTemplate, &resources); err != nil {
		return err
	}
	for _, item := range []struct {
		name       string
		visibility ebsv1.ConfigVisibility
		value      interface{}
	}{
		{ebsv1.BuildTargetConfigName, ebsv1.ConfigVisibilityPublic, target},
		{ebsv1.BuildResourceConfigName, ebsv1.ConfigVisibilityOpsOnly, resources},
	} {
		content, err := yaml.Marshal(item.value)
		if err != nil {
			return err
		}
		if err := ensureDefaultConfig(ctx, storage, &ebsv1.Config{
			TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Config"},
			ObjectMeta: metav1.ObjectMeta{Name: item.name},
			Spec:       ebsv1.ConfigSpec{Visibility: item.visibility, Content: string(content)},
		}); err != nil {
			return err
		}
	}
	return nil
}

func ensureDefaultConfig(ctx context.Context, storage bootstrapStorage, obj *ebsv1.Config) error {
	ctx = apirequest.WithNamespace(ctx, "")
	if _, err := storage.Get(ctx, obj.Name, &metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if _, err := storage.Create(ctx, obj, nil, &metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, err = storage.Get(ctx, obj.Name, &metav1.GetOptions{})
		return err
	} else {
		return err
	}
}

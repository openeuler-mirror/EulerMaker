package server

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
)

const defaultBuildResourceName = "default"

//go:embed default-build-resource.json
var defaultBuildResourceTemplate []byte

type defaultBuildResourceStorage interface {
	rest.Getter
	rest.Creater
}

func ensureDefaultBuildResource(ctx context.Context, storage defaultBuildResourceStorage) error {
	ctx = genericapirequest.WithNamespace(ctx, defaultBuildResourceName)
	_, err := storage.Get(ctx, defaultBuildResourceName, &metav1.GetOptions{})
	switch {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("get default BuildResource: %w", err)
	}

	obj := new(ebsv1.BuildResource)
	decoder := json.NewDecoder(bytes.NewReader(defaultBuildResourceTemplate))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(obj); err != nil {
		return fmt.Errorf("decode embedded default BuildResource: %w", err)
	}
	_, err = storage.Create(ctx, obj, nil, &metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create default BuildResource: %w", err)
	}
	return nil
}

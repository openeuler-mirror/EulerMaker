package server

import (
	"context"
	"fmt"
	"io"
	"os"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

func ensureDefaultScript(ctx context.Context, storage defaultBuildResourceConfigStorage, filename string) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxScriptRequestSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxScriptRequestSize {
		return fmt.Errorf("Script initialization file exceeds request limit")
	}
	data, err = yaml.YAMLToJSONStrict(data)
	if err != nil {
		return err
	}
	obj, err := decodeScript(data)
	if err != nil {
		return err
	}
	if errs := validation.ValidateScript(obj); len(errs) > 0 {
		return apierrors.NewInvalid(ebsv1.Kind("Script"), obj.Name, errs)
	}
	ctx = apirequest.WithNamespace(ctx, "")
	if _, err := storage.Get(ctx, obj.Name, &metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		return err
	}
	_, err = storage.Create(ctx, obj, nil, &metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		_, err = storage.Get(ctx, obj.Name, &metav1.GetOptions{})
	}
	return err
}

package server

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"sigs.k8s.io/yaml"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/apis/ebs/validation"
)

const maxBootstrapScriptSize = 2 * 1024 * 1024

//go:embed default-rpmbuild-script.yaml
var defaultRpmbuildScript []byte

func decodeScript(data []byte) (*ebsv1.Script, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("Script must be UTF-8 JSON")
	}
	if len(data) > maxBootstrapScriptSize {
		return nil, fmt.Errorf("Script exceeds request limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	obj := new(ebsv1.Script)
	if err := decoder.Decode(obj); err != nil {
		return nil, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("decode trailing data: %w", err)
		}
		return nil, fmt.Errorf("request body must contain exactly one object")
	}
	if obj.Kind != "" && obj.Kind != "Script" || obj.APIVersion != "" && obj.APIVersion != "ebs/v1" {
		return nil, fmt.Errorf("expected ebs/v1 Script")
	}
	obj.Kind, obj.APIVersion = "Script", "ebs/v1"
	if obj.Namespace != "" || obj.GenerateName != "" {
		return nil, fmt.Errorf("Script does not support namespace or generateName")
	}
	return obj, nil
}

func ensureDefaultScript(ctx context.Context, storage bootstrapStorage) error {
	return ensureScript(ctx, storage, defaultRpmbuildScript)
}

func ensureScript(ctx context.Context, storage bootstrapStorage, data []byte) error {
	if len(data) > maxBootstrapScriptSize {
		return fmt.Errorf("Script initialization file exceeds request limit")
	}
	data, err := yaml.YAMLToJSONStrict(data)
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

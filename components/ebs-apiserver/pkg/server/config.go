package server

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/esstore"
	"github.com/emicklei/go-restful/v3"
	jsonpatch "github.com/evanphx/json-patch"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"sigs.k8s.io/yaml"
)

const maxConfigRequestSize = 16 << 20

func installConfigRoutes(srv handlerServer, store *esstore.Store) error {
	ws := ebsV1WebService(srv)
	if ws == nil {
		return fmt.Errorf("ebs/v1 web service is not installed")
	}
	h := &configHandler{store: store}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		route := ws.Method(method).Path("/configs").To(h.handle).Produces(restful.MIME_JSON).Writes(ebsv1.ConfigList{})
		if method == http.MethodPost {
			route.Reads(ebsv1.Config{}).Writes(ebsv1.Config{})
		}
		ws.Route(route)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch} {
		ws.Route(ws.Method(method).Path("/configs/{name}").To(h.handle).Produces(restful.MIME_JSON).Writes(ebsv1.Config{}))
	}
	return nil
}

type configHandler struct{ store *esstore.Store }

func (h *configHandler) handle(req *restful.Request, resp *restful.Response) {
	if err := h.serve(req, resp); err != nil {
		writeBuildResourceConfigError(resp, err)
	}
}

func (h *configHandler) serve(req *restful.Request, resp *restful.Response) error {
	ctx := apirequest.WithNamespace(req.Request.Context(), "")
	name, method := req.PathParameter("name"), req.Request.Method
	if _, exists := req.Request.URL.Query()["watch"]; exists && req.QueryParameter("watch") != "false" {
		return apierrors.NewBadRequest("Config does not support watch")
	}
	if method == http.MethodGet || method == http.MethodHead {
		var obj interface{}
		var err error
		if name == "" {
			listOpts, listErr := resourceListOptions(req)
			if listErr != nil {
				return apierrors.NewBadRequest(listErr.Error())
			}
			obj, err = h.store.List(ctx, listOpts)
			if err == nil {
				obj.(*ebsv1.ConfigList).GetObjectKind().SetGroupVersionKind(ebsv1.SchemeGroupVersion.WithKind("ConfigList"))
			}
		} else {
			obj, err = h.store.Get(ctx, name, &metav1.GetOptions{})
		}
		if err != nil {
			return err
		}
		if method == http.MethodHead {
			resp.WriteHeader(http.StatusOK)
			return nil
		}
		return resp.WriteEntity(obj)
	}
	if req.QueryParameter("dryRun") != "" {
		return apierrors.NewBadRequest("dryRun is not supported by Config")
	}
	data, err := io.ReadAll(io.LimitReader(req.Request.Body, maxConfigRequestSize+1))
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if len(data) > maxConfigRequestSize {
		return apierrors.NewRequestEntityTooLargeError("Config exceeds request limit")
	}
	var patchVersion string
	if method == http.MethodPatch {
		old, err := h.store.Get(ctx, name, &metav1.GetOptions{})
		if err != nil {
			return err
		}
		oldData, err := json.Marshal(old)
		if err != nil {
			return err
		}
		patchVersion = old.(*ebsv1.Config).ResourceVersion
		switch strings.TrimSpace(strings.Split(req.HeaderParameter("Content-Type"), ";")[0]) {
		case "application/merge-patch+json":
			data, err = jsonpatch.MergePatch(oldData, data)
		case "application/json-patch+json":
			var patch jsonpatch.Patch
			patch, err = jsonpatch.DecodePatch(data)
			if err == nil {
				data, err = patch.Apply(oldData)
			}
		default:
			return apierrors.NewBadRequest("only JSON Patch and JSON Merge Patch are supported")
		}
		if err != nil {
			return apierrors.NewBadRequest(err.Error())
		}
	}
	obj, err := decodeConfig(data)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if name != "" && obj.Name != name {
		return apierrors.NewBadRequest("metadata.name must match request path")
	}
	if method == http.MethodPatch && obj.ResourceVersion != patchVersion {
		return apierrors.NewConflict(ebsv1.Resource("configs"), name, fmt.Errorf("resourceVersion does not match the patched object"))
	}
	if method == http.MethodPost {
		out, err := h.store.Create(ctx, obj, nil, &metav1.CreateOptions{})
		if err != nil {
			return err
		}
		return resp.WriteHeaderAndEntity(http.StatusCreated, out)
	}
	out, _, err := h.store.Update(ctx, name, rest.DefaultUpdatedObjectInfo(obj), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return resp.WriteEntity(out)
}

func decodeConfig(data []byte) (*ebsv1.Config, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("Config must be UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	obj := new(ebsv1.Config)
	if err := decoder.Decode(obj); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if obj.Kind != "" && obj.Kind != "Config" || obj.APIVersion != "" && obj.APIVersion != "ebs/v1" {
		return nil, fmt.Errorf("expected ebs/v1 Config")
	}
	obj.Kind, obj.APIVersion = "Config", "ebs/v1"
	return obj, nil
}

func ensureDefaultConfigs(ctx context.Context, storage defaultBuildResourceConfigStorage) error {
	var target ebsv1.BuildConf
	if err := yaml.UnmarshalStrict(defaultBuildConfTemplate, &target); err != nil {
		return err
	}
	var resources ebsv1.BuildResourceConfig
	if err := yaml.UnmarshalStrict(defaultBuildResourceConfigTemplate, &resources); err != nil {
		return err
	}
	for _, item := range []struct {
		name       string
		visibility ebsv1.ConfigVisibility
		value      interface{}
	}{
		{ebsv1.BuildTargetConfigName, ebsv1.ConfigVisibilityPublic, target.Spec},
		{ebsv1.BuildResourceConfigName, ebsv1.ConfigVisibilityOpsOnly, resources.Spec},
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

func ensureDefaultConfig(ctx context.Context, storage defaultBuildResourceConfigStorage, obj *ebsv1.Config) error {
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

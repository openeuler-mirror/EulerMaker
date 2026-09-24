package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/emicklei/go-restful/v3"
	jsonpatch "github.com/evanphx/json-patch"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/esstore"
)

const maxScriptRequestSize = 2 * 1024 * 1024

func installScriptRoutes(srv handlerServer, store *esstore.Store) error {
	ws := ebsV1WebService(srv)
	if ws == nil {
		return fmt.Errorf("ebs/v1 web service is not installed")
	}
	h := &scriptHandler{store}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		route := ws.Method(method).Path("/scripts").To(h.handle).Produces(restful.MIME_JSON).Writes(ebsv1.ScriptList{})
		if method == http.MethodPost {
			route.Reads(ebsv1.Script{}).Writes(ebsv1.Script{})
		}
		ws.Route(route)
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch} {
		ws.Route(ws.Method(method).Path("/scripts/{name}").To(h.handle).Produces(restful.MIME_JSON).Writes(ebsv1.Script{}))
	}
	return nil
}

type scriptHandler struct{ store *esstore.Store }

func (h *scriptHandler) handle(req *restful.Request, resp *restful.Response) {
	if err := h.serve(req, resp); err != nil {
		writeBuildResourceConfigError(resp, err)
	}
}

func (h *scriptHandler) serve(req *restful.Request, resp *restful.Response) error {
	ctx := apirequest.WithNamespace(req.Request.Context(), "")
	name := req.PathParameter("name")
	if values, exists := req.Request.URL.Query()["watch"]; exists {
		for _, value := range values {
			if value != "false" {
				return apierrors.NewBadRequest("Script does not support watch")
			}
		}
	}
	method := req.Request.Method
	var patchVersion string
	if method == http.MethodGet || method == http.MethodHead {
		if name == "" {
			opts, err := resourceListOptions(req)
			if err != nil {
				return apierrors.NewBadRequest(err.Error())
			}
			obj, err := h.store.List(ctx, opts)
			if err != nil {
				return err
			}
			obj.GetObjectKind().SetGroupVersionKind(ebsv1.SchemeGroupVersion.WithKind("ScriptList"))
			if method == http.MethodHead {
				resp.WriteHeader(http.StatusOK)
				return nil
			}
			return resp.WriteEntity(obj)
		}
		obj, err := h.store.Get(ctx, name, &metav1.GetOptions{})
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
		return apierrors.NewBadRequest("dryRun is not supported by Script")
	}
	data, err := io.ReadAll(io.LimitReader(req.Request.Body, maxScriptRequestSize+1))
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if len(data) > maxScriptRequestSize {
		return apierrors.NewRequestEntityTooLargeError("Script exceeds request limit")
	}
	if !utf8.Valid(data) {
		return apierrors.NewBadRequest("Script must be UTF-8 JSON")
	}
	if method == http.MethodPatch {
		old, err := h.store.Get(ctx, name, &metav1.GetOptions{})
		if err != nil {
			return err
		}
		oldData, err := json.Marshal(old)
		patchVersion = old.(*ebsv1.Script).ResourceVersion
		if err != nil {
			return err
		}
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
	obj, err := decodeScript(data)
	if err != nil {
		return apierrors.NewBadRequest(err.Error())
	}
	if name != "" && name != obj.Name {
		return apierrors.NewBadRequest("metadata.name must match request path")
	}
	if method == http.MethodPatch && obj.ResourceVersion != patchVersion {
		return apierrors.NewConflict(ebsv1.Resource("scripts"), name, fmt.Errorf("resourceVersion does not match the patched object"))
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

func decodeScript(data []byte) (*ebsv1.Script, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("Script must be UTF-8 JSON")
	}
	if len(data) > maxScriptRequestSize {
		return nil, fmt.Errorf("Script exceeds request limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	obj := new(ebsv1.Script)
	if err := decoder.Decode(obj); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
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

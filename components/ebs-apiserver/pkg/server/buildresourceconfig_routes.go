package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/emicklei/go-restful/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	ebsv1 "ebs-api/ebs/v1"
)

const maxBuildResourceConfigRequestSize = 16 << 20

type buildResourceConfigStorage interface {
	rest.Getter
	rest.Lister
	rest.Creater
	rest.Updater
	rest.GracefulDeleter
}

func installBuildResourceConfigRoutes(srv handlerServer, storage buildResourceConfigStorage) error {
	ws := ebsV1WebService(srv)
	if ws == nil {
		return fmt.Errorf("ebs/v1 web service is not installed")
	}
	handler := &buildResourceConfigHandler{storage: storage}
	base := "/buildresourceconfigs"
	ws.Route(ws.GET(base).To(handler.list))
	ws.Route(ws.POST(base).To(handler.create))
	ws.Route(ws.GET(base + "/{name}").To(handler.get))
	ws.Route(ws.PUT(base + "/{name}").To(handler.update))
	ws.Route(ws.DELETE(base + "/{name}").To(handler.delete))
	return nil
}

type handlerServer interface {
	RegisteredWebServices() []*restful.WebService
}

func ebsV1WebService(server handlerServer) *restful.WebService {
	for _, ws := range server.RegisteredWebServices() {
		if ws.RootPath() == "/apis/ebs/v1" {
			return ws
		}
	}
	return nil
}

type buildResourceConfigHandler struct{ storage buildResourceConfigStorage }

func (h *buildResourceConfigHandler) list(req *restful.Request, resp *restful.Response) {
	ctx, _ := buildResourceConfigRequest(req, false)
	options, err := resourceListOptions(req)
	if err != nil {
		writeBuildResourceConfigError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	obj, err := h.storage.List(ctx, options)
	if err != nil {
		writeBuildResourceConfigError(resp, err)
		return
	}
	_ = resp.WriteEntity(obj)
}

func (h *buildResourceConfigHandler) get(req *restful.Request, resp *restful.Response) {
	ctx, name := buildResourceConfigRequest(req, true)
	obj, err := h.storage.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		writeBuildResourceConfigError(resp, err)
		return
	}
	_ = resp.WriteEntity(obj)
}

func (h *buildResourceConfigHandler) create(req *restful.Request, resp *restful.Response) {
	ctx, _ := buildResourceConfigRequest(req, false)
	obj, err := decodeBuildResourceConfig(req.Request.Body)
	if err != nil {
		writeBuildResourceConfigError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	if err := normalizeBuildResourceConfigIdentity(obj, obj.Name); err != nil {
		writeBuildResourceConfigError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	created, err := h.storage.Create(ctx, obj, nil, &metav1.CreateOptions{})
	if err != nil {
		writeBuildResourceConfigError(resp, err)
		return
	}
	resp.WriteHeaderAndEntity(http.StatusCreated, created)
}

func (h *buildResourceConfigHandler) update(req *restful.Request, resp *restful.Response) {
	ctx, name := buildResourceConfigRequest(req, true)
	obj, err := decodeBuildResourceConfig(req.Request.Body)
	if err != nil {
		writeBuildResourceConfigError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	if err := normalizeBuildResourceConfigIdentity(obj, name); err != nil {
		writeBuildResourceConfigError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	updated, _, err := h.storage.Update(ctx, name, rest.DefaultUpdatedObjectInfo(obj), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		writeBuildResourceConfigError(resp, err)
		return
	}
	_ = resp.WriteEntity(updated)
}

func (h *buildResourceConfigHandler) delete(req *restful.Request, resp *restful.Response) {
	ctx, name := buildResourceConfigRequest(req, true)
	deleted, _, err := h.storage.Delete(ctx, name, nil, &metav1.DeleteOptions{})
	if err != nil {
		writeBuildResourceConfigError(resp, err)
		return
	}
	_ = resp.WriteEntity(deleted)
}

func buildResourceConfigRequest(req *restful.Request, item bool) (context.Context, string) {
	name := ""
	if item {
		name = req.PathParameter("name")
	}
	return genericapirequest.WithNamespace(req.Request.Context(), ""), name
}

func normalizeBuildResourceConfigIdentity(obj *ebsv1.BuildResourceConfig, name string) error {
	if obj.Name != "" && obj.Name != name {
		return fmt.Errorf("metadata.name must equal %q", name)
	}
	if obj.Namespace != "" || obj.GenerateName != "" {
		return fmt.Errorf("BuildResourceConfig does not support namespace or generateName")
	}
	obj.Name = name
	obj.APIVersion = ebsv1.SchemeGroupVersion.String()
	obj.Kind = "BuildResourceConfig"
	return nil
}

func decodeBuildResourceConfig(body io.ReadCloser) (*ebsv1.BuildResourceConfig, error) {
	defer body.Close()
	decoder := json.NewDecoder(io.LimitReader(body, maxBuildResourceConfigRequestSize+1))
	decoder.DisallowUnknownFields()
	var obj ebsv1.BuildResourceConfig
	if err := decoder.Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode BuildResourceConfig: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	return &obj, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode trailing data: %w", err)
	}
	return fmt.Errorf("request body must contain exactly one object")
}

func resourceListOptions(req *restful.Request) (*internalversion.ListOptions, error) {
	options := &internalversion.ListOptions{}
	var err error
	if value := req.QueryParameter("labelSelector"); value != "" {
		options.LabelSelector, err = labels.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("invalid labelSelector: %w", err)
		}
	}
	if value := req.QueryParameter("fieldSelector"); value != "" {
		options.FieldSelector, err = fields.ParseSelector(value)
		if err != nil {
			return nil, fmt.Errorf("invalid fieldSelector: %w", err)
		}
	}
	if value := req.QueryParameter("limit"); value != "" {
		options.Limit, err = strconv.ParseInt(value, 10, 64)
		if err != nil || options.Limit < 0 {
			return nil, fmt.Errorf("limit must be a non-negative integer")
		}
	}
	options.Continue = req.QueryParameter("continue")
	return options, nil
}

func writeBuildResourceConfigError(resp *restful.Response, err error) {
	status := apierrors.NewInternalError(err).ErrStatus
	if apiStatus, ok := err.(apierrors.APIStatus); ok {
		status = apiStatus.Status()
	}
	resp.WriteHeaderAndEntity(int(status.Code), &status)
}

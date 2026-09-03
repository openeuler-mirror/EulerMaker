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

	ebsv1 "ebs-apiserver/pkg/apis/ebs/v1"
)

const maxBuildResourceRequestSize = 16 << 20

type buildResourceStorage interface {
	rest.Getter
	rest.Lister
	rest.Creater
	rest.Updater
	rest.GracefulDeleter
}

func installBuildResourceRoutes(srv handlerServer, storage buildResourceStorage) error {
	ws := ebsV1WebService(srv)
	if ws == nil {
		return fmt.Errorf("ebs/v1 web service is not installed")
	}
	handler := &buildResourceHandler{storage: storage}
	base := "/projects/{project}/buildresources"
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

type buildResourceHandler struct{ storage buildResourceStorage }

func (h *buildResourceHandler) list(req *restful.Request, resp *restful.Response) {
	ctx, _, ok := buildResourceRequest(req, resp, false)
	if !ok {
		return
	}
	options, err := buildResourceListOptions(req)
	if err != nil {
		writeBuildResourceError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	obj, err := h.storage.List(ctx, options)
	if err != nil {
		writeBuildResourceError(resp, err)
		return
	}
	_ = resp.WriteEntity(obj)
}

func (h *buildResourceHandler) get(req *restful.Request, resp *restful.Response) {
	ctx, name, ok := buildResourceRequest(req, resp, true)
	if !ok {
		return
	}
	obj, err := h.storage.Get(ctx, name, &metav1.GetOptions{})
	if err != nil {
		writeBuildResourceError(resp, err)
		return
	}
	_ = resp.WriteEntity(obj)
}

func (h *buildResourceHandler) create(req *restful.Request, resp *restful.Response) {
	ctx, project, ok := buildResourceRequest(req, resp, false)
	if !ok {
		return
	}
	obj, err := decodeBuildResource(req.Request.Body)
	if err != nil {
		writeBuildResourceError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	if err := normalizeBuildResourceIdentity(obj, project, obj.Name); err != nil {
		writeBuildResourceError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	created, err := h.storage.Create(ctx, obj, nil, &metav1.CreateOptions{})
	if err != nil {
		writeBuildResourceError(resp, err)
		return
	}
	resp.WriteHeaderAndEntity(http.StatusCreated, created)
}

func (h *buildResourceHandler) update(req *restful.Request, resp *restful.Response) {
	ctx, name, ok := buildResourceRequest(req, resp, true)
	if !ok {
		return
	}
	obj, err := decodeBuildResource(req.Request.Body)
	if err != nil {
		writeBuildResourceError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	project, _ := genericapirequest.NamespaceFrom(ctx)
	if err := normalizeBuildResourceIdentity(obj, project, name); err != nil {
		writeBuildResourceError(resp, apierrors.NewBadRequest(err.Error()))
		return
	}
	updated, _, err := h.storage.Update(ctx, name, rest.DefaultUpdatedObjectInfo(obj), nil, nil, false, &metav1.UpdateOptions{})
	if err != nil {
		writeBuildResourceError(resp, err)
		return
	}
	_ = resp.WriteEntity(updated)
}

func (h *buildResourceHandler) delete(req *restful.Request, resp *restful.Response) {
	ctx, name, ok := buildResourceRequest(req, resp, true)
	if !ok {
		return
	}
	deleted, _, err := h.storage.Delete(ctx, name, nil, &metav1.DeleteOptions{})
	if err != nil {
		writeBuildResourceError(resp, err)
		return
	}
	_ = resp.WriteEntity(deleted)
}

func buildResourceRequest(req *restful.Request, resp *restful.Response, item bool) (context.Context, string, bool) {
	project := req.PathParameter("project")
	if project == "" {
		writeBuildResourceError(resp, apierrors.NewBadRequest("project is required"))
		return nil, "", false
	}
	name := project
	if item {
		name = req.PathParameter("name")
	}
	return genericapirequest.WithNamespace(req.Request.Context(), project), name, true
}

func normalizeBuildResourceIdentity(obj *ebsv1.BuildResource, project, name string) error {
	if obj.Name != "" && obj.Name != name {
		return fmt.Errorf("metadata.name must equal %q", name)
	}
	if obj.Namespace != "" && obj.Namespace != project {
		return fmt.Errorf("metadata.namespace must equal %q", project)
	}
	obj.Name = name
	obj.Namespace = project
	obj.APIVersion = ebsv1.SchemeGroupVersion.String()
	obj.Kind = "BuildResource"
	return nil
}

func decodeBuildResource(body io.ReadCloser) (*ebsv1.BuildResource, error) {
	defer body.Close()
	decoder := json.NewDecoder(io.LimitReader(body, maxBuildResourceRequestSize+1))
	decoder.DisallowUnknownFields()
	var obj ebsv1.BuildResource
	if err := decoder.Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode BuildResource: %w", err)
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

func buildResourceListOptions(req *restful.Request) (*internalversion.ListOptions, error) {
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

func writeBuildResourceError(resp *restful.Response, err error) {
	status := apierrors.NewInternalError(err).ErrStatus
	if apiStatus, ok := err.(apierrors.APIStatus); ok {
		status = apiStatus.Status()
	}
	resp.WriteHeaderAndEntity(int(status.Code), &status)
}

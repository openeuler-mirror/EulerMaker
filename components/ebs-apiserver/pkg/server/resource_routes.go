package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/emicklei/go-restful/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apiserver/pkg/registry/rest"
)

type handlerServer interface {
	RegisteredWebServices() []*restful.WebService
}

type bootstrapStorage interface {
	rest.Getter
	rest.Creater
}

func ebsV1WebService(server handlerServer) *restful.WebService {
	for _, ws := range server.RegisteredWebServices() {
		if ws.RootPath() == "/apis/ebs/v1" {
			return ws
		}
	}
	return nil
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

func writeResourceError(resp *restful.Response, err error) {
	status := apierrors.NewInternalError(err).ErrStatus
	if apiStatus, ok := err.(apierrors.APIStatus); ok {
		status = apiStatus.Status()
	}
	resp.WriteHeaderAndEntity(int(status.Code), &status)
}

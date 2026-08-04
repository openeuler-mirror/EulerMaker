package iam

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/emicklei/go-restful/v3"
	genericapiserver "k8s.io/apiserver/pkg/server"

	"ebs-apiserver/pkg/iam/credential"
)

const maxRequestBody = 1 << 20

type Handler struct{ credentials *credential.Store }

func InstallInternalRoutes(server *genericapiserver.GenericAPIServer, credentials *credential.Store) {
	h := &Handler{credentials: credentials}
	ws := new(restful.WebService).Path("/internal/iam/v1").Consumes(restful.MIME_JSON).Produces(restful.MIME_JSON)
	ws.Route(ws.POST("/authenticate").To(h.authenticate))
	ws.Route(ws.PUT("/users/{name}/password").To(h.setPassword))
	server.Handler.GoRestfulContainer.Add(ws)
}

func (h *Handler) authenticate(req *restful.Request, resp *restful.Response) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(req.Request, &body); err != nil || body.Username == "" || body.Password == "" {
		writeError(resp, http.StatusBadRequest, "invalid request")
		return
	}
	ok, err := h.credentials.Authenticate(req.Request.Context(), body.Username, body.Password)
	if err != nil {
		writeError(resp, http.StatusInternalServerError, "authentication unavailable")
		return
	}
	if !ok {
		writeError(resp, http.StatusUnauthorized, "authentication failed")
		return
	}
	_ = resp.WriteHeaderAndEntity(http.StatusOK, map[string]interface{}{"authenticated": true, "username": body.Username})
}

func (h *Handler) setPassword(req *restful.Request, resp *restful.Response) {
	name := req.PathParameter("name")
	var body struct {
		Password string `json:"password"`
	}
	if err := decode(req.Request, &body); err != nil || name == "" {
		writeError(resp, http.StatusBadRequest, "invalid request")
		return
	}
	if err := h.credentials.SetPassword(req.Request.Context(), name, body.Password); err != nil {
		if errors.Is(err, credential.ErrInvalidPassword) {
			writeError(resp, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, credential.ErrUserNotFound) {
			writeError(resp, http.StatusNotFound, "user not found")
			return
		}
		writeError(resp, http.StatusInternalServerError, "unable to set password")
		return
	}
	resp.WriteHeader(http.StatusNoContent)
}

func decode(req *http.Request, out interface{}) error {
	defer req.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(req.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

func writeError(resp *restful.Response, status int, message string) {
	_ = resp.WriteHeaderAndEntity(status, map[string]string{"error": message})
}

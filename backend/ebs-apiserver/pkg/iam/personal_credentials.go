package iam

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"github.com/emicklei/go-restful/v3"
	"k8s.io/klog/v2"

	"ebs-apiserver/pkg/iam/credential"
)

func (h *Handler) createPersonalCredential(req *restful.Request, resp *restful.Response) {
	resp.AddHeader("Cache-Control", "no-store")
	if !h.requireGateway(req, resp) {
		return
	}
	defer req.Request.Body.Close()
	data, err := io.ReadAll(io.LimitReader(req.Request.Body, maxRequestBody+1))
	if err != nil || len(data) > maxRequestBody || !utf8.Valid(data) || !json.Valid(data) || !validJSONUnicode(data) {
		writePersonalError(resp, credential.ErrPersonalInput)
		return
	}
	var body struct {
		Name          string          `json:"name"`
		ExpiresInDays json.RawMessage `json:"expiresInDays"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writePersonalError(resp, credential.ErrPersonalInput)
		return
	}
	var days *int
	if len(body.ExpiresInDays) > 0 {
		var value int
		if bytes.Equal(body.ExpiresInDays, []byte("null")) || json.Unmarshal(body.ExpiresInDays, &value) != nil {
			writePersonalError(resp, credential.ErrPersonalInput)
			return
		}
		days = &value
	}
	result, err := h.credentials.CreatePersonal(req.Request.Context(), req.PathParameter("name"), body.Name, days)
	if err != nil {
		writePersonalError(resp, err)
		return
	}
	_ = resp.WriteHeaderAndEntity(http.StatusCreated, result)
}

func (h *Handler) listPersonalCredentials(req *restful.Request, resp *restful.Response) {
	if !h.personalReadRequest(req, resp) {
		return
	}
	items, err := h.credentials.ListPersonal(req.Request.Context(), req.PathParameter("name"))
	if err != nil {
		writePersonalError(resp, err)
		return
	}
	_ = resp.WriteEntity(map[string]interface{}{"items": items})
}

func (h *Handler) deletePersonalCredential(req *restful.Request, resp *restful.Response) {
	if !h.personalReadRequest(req, resp) {
		return
	}
	if err := h.credentials.DeletePersonal(req.Request.Context(), req.PathParameter("name"), req.PathParameter("id")); err != nil {
		writePersonalError(resp, err)
		return
	}
	resp.WriteHeader(http.StatusNoContent)
}

func (h *Handler) personalReadRequest(req *restful.Request, resp *restful.Response) bool {
	resp.AddHeader("Cache-Control", "no-store")
	if !h.requireGateway(req, resp) {
		return false
	}
	defer req.Request.Body.Close()
	data, err := io.ReadAll(io.LimitReader(req.Request.Body, 1))
	if err != nil || len(data) > 0 {
		writePersonalError(resp, credential.ErrPersonalInput)
		return false
	}
	return true
}

func (h *Handler) authenticatePersonalCredential(req *restful.Request, resp *restful.Response, token string) {
	if !h.requireGateway(req, resp) {
		return
	}
	result, err := h.credentials.AuthenticatePersonal(req.Request.Context(), token)
	if err != nil {
		writePersonalError(resp, err)
		return
	}
	_ = resp.WriteEntity(result)
}

func writePersonalError(resp *restful.Response, err error) {
	status, code, message := http.StatusServiceUnavailable, "CredentialServiceUnavailable", "credential service unavailable"
	switch {
	case errors.Is(err, credential.ErrPersonalInput):
		status, code, message = http.StatusBadRequest, "InvalidRequest", "invalid request"
	case errors.Is(err, credential.ErrPersonalInvalid):
		status, code, message = http.StatusUnauthorized, "InvalidCredential", "invalid credential"
	case errors.Is(err, credential.ErrUserNotFound):
		status, code, message = http.StatusNotFound, "UserNotFound", "user not found"
	case errors.Is(err, credential.ErrUserUnavailable):
		status, code, message = http.StatusForbidden, "UserUnavailable", "user unavailable"
	case errors.Is(err, credential.ErrPersonalLimit):
		status, code, message = http.StatusConflict, "CredentialLimitExceeded", "at most 5 active credentials are allowed"
	case errors.Is(err, credential.ErrPersonalConflict):
		status, code, message = http.StatusConflict, "CredentialConflict", "credential update conflict"
	}
	if status >= 500 {
		// Raw storage errors can contain credentials from the request document.
		klog.ErrorS(nil, "Personal credential operation failed", "code", code)
	}
	_ = resp.WriteHeaderAndEntity(status, map[string]string{"code": code, "error": message})
}

package iam

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"

	"github.com/emicklei/go-restful/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
	"ebs-apiserver/pkg/iam/credential"
)

const maxRequestBody = 1 << 20

type userStorage interface {
	CreateWithCredential(context.Context, runtime.Object, json.RawMessage, rest.ValidateObjectFunc, *metav1.CreateOptions) (runtime.Object, error)
}

type Handler struct {
	credentials *credential.Store
	users       userStorage
	machines    userStorage
}

func InstallInternalRoutes(server *genericapiserver.GenericAPIServer, credentials *credential.Store, users, machines userStorage) {
	h := &Handler{credentials: credentials, users: users, machines: machines}
	ws := new(restful.WebService).Path("/internal/iam/v1").Consumes(restful.MIME_JSON).Produces(restful.MIME_JSON)
	ws.Route(ws.POST("/users/register").To(h.register))
	ws.Route(ws.POST("/authenticate").To(h.authenticate))
	ws.Route(ws.PUT("/users/{name}/password").To(h.setPassword))
	ws.Route(ws.POST("/machineaccounts/register").To(h.registerMachine))
	ws.Route(ws.POST("/machineaccounts/{name}/authenticate").To(h.authenticateMachine))
	server.Handler.GoRestfulContainer.Add(ws)
}

func (h *Handler) registerMachine(req *restful.Request, resp *restful.Response) {
	var body struct {
		Name            string `json:"name"`
		ClientSecret    string `json:"clientSecret"`
		TokenTTLSeconds int64  `json:"tokenTTLSeconds"`
	}
	if err := decode(req.Request, &body); err != nil || body.Name == "" || len(utilvalidation.IsDNS1123Label(body.Name)) != 0 {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}
	if body.TokenTTLSeconds == 0 {
		body.TokenTTLSeconds = 3600
	}
	credentialData, err := credential.NewMachineCredential(body.ClientSecret)
	if err != nil || body.TokenTTLSeconds < 300 || body.TokenTTLSeconds > 86400 {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}
	account := &iamv1.MachineAccount{TypeMeta: metav1.TypeMeta{APIVersion: iamv1.SchemeGroupVersion.String(), Kind: "MachineAccount"}, ObjectMeta: metav1.ObjectMeta{Name: body.Name}, Spec: iamv1.MachineAccountSpec{TokenTTLSeconds: body.TokenTTLSeconds}}
	ctx := genericapirequest.WithNamespace(req.Request.Context(), "")
	if _, err := h.machines.CreateWithCredential(ctx, account, credentialData, nil, &metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(resp, http.StatusConflict, "machine account already exists")
		} else {
			klog.ErrorS(err, "Unable to register machine account", "name", body.Name)
			writeError(resp, http.StatusInternalServerError, "registration unavailable")
		}
		return
	}
	_ = resp.WriteHeaderAndEntity(http.StatusCreated, map[string]string{"name": body.Name})
}

func (h *Handler) authenticateMachine(req *restful.Request, resp *restful.Response) {
	name := req.PathParameter("name")
	var body struct {
		ClientSecret string `json:"clientSecret"`
	}
	if err := decode(req.Request, &body); err != nil || name == "" || body.ClientSecret == "" {
		writeError(resp, http.StatusBadRequest, "invalid request")
		return
	}
	ttl, ok, err := h.credentials.AuthenticateMachine(req.Request.Context(), name, body.ClientSecret)
	if err != nil {
		writeError(resp, http.StatusInternalServerError, "authentication unavailable")
		return
	}
	if !ok {
		writeError(resp, http.StatusUnauthorized, "authentication failed")
		return
	}
	_ = resp.WriteHeaderAndEntity(http.StatusOK, map[string]interface{}{"authenticated": true, "name": name, "tokenTTLSeconds": ttl})
}

func (h *Handler) register(req *restful.Request, resp *restful.Response) {
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"displayName"`
		Email       string `json:"email"`
	}
	if err := decode(req.Request, &body); err != nil || !validRegistration(body.Username, body.Password, body.Email) {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}

	enabled := true
	user := &iamv1.User{
		TypeMeta:   metav1.TypeMeta{APIVersion: iamv1.SchemeGroupVersion.String(), Kind: "User"},
		ObjectMeta: metav1.ObjectMeta{Name: body.Username},
		Spec:       iamv1.UserSpec{Enabled: &enabled, DisplayName: body.DisplayName, Email: body.Email},
	}
	credentialData, err := credential.NewPasswordCredential(body.Password)
	if err != nil {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}
	ctx := genericapirequest.WithNamespace(req.Request.Context(), "")
	_, err = h.users.CreateWithCredential(ctx, user, credentialData, nil, &metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(resp, http.StatusConflict, "username already exists")
			return
		}
		klog.ErrorS(err, "Unable to register user", "username", body.Username)
		writeError(resp, http.StatusInternalServerError, "registration unavailable")
		return
	}
	_ = resp.WriteHeaderAndEntity(http.StatusCreated, map[string]string{"username": body.Username})
}

func validRegistration(username, password, email string) bool {
	if username == "" || len(utilvalidation.IsDNS1123Label(username)) != 0 {
		return false
	}
	if err := credential.ValidatePassword(password); err != nil {
		return false
	}
	if email != "" {
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email {
			return false
		}
	}
	return true
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
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}

func writeError(resp *restful.Response, status int, message string) {
	_ = resp.WriteHeaderAndEntity(status, map[string]string{"error": message})
}

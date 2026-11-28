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

	iamv1 "ebs-api/iam/v1"
	iamvalidation "ebs-apiserver/pkg/apis/iam/validation"
	"ebs-apiserver/pkg/iam/credential"
	"ebs-apiserver/pkg/storage/es"
)

const maxRequestBody = 1 << 20

type userStorage interface {
	CreateWithCredential(context.Context, runtime.Object, json.RawMessage, rest.ValidateObjectFunc, *metav1.CreateOptions) (runtime.Object, error)
}

type Handler struct {
	credentials *credential.Store
	users       userStorage
	machines    userStorage
	oauth       *oauthService
}

func InstallInternalRoutes(server *genericapiserver.GenericAPIServer, credentials *credential.Store, users interface {
	userStorage
	oauthUserStorage
}, machines userStorage, client *es.Client) {
	h := &Handler{credentials: credentials, users: users, machines: machines, oauth: &oauthService{client: client, users: users}}
	ws := new(restful.WebService).Path("/internal/iam/v1").Consumes(restful.MIME_JSON).Produces(restful.MIME_JSON)
	ws.Route(ws.POST("/users/register").To(h.register))
	ws.Route(ws.POST("/users/authenticate").To(h.authenticate))
	ws.Route(ws.PUT("/users/{name}/password").To(h.setPassword))
	ws.Route(ws.POST("/users/{name}/credentials").To(h.createPersonalCredential))
	ws.Route(ws.GET("/users/{name}/credentials").To(h.listPersonalCredentials))
	ws.Route(ws.DELETE("/users/{name}/credentials/{id}").To(h.deletePersonalCredential))
	ws.Route(ws.POST("/machineaccounts/register").To(h.registerMachine))
	ws.Route(ws.POST("/machineaccounts/authenticate").To(h.authenticateMachine))
	server.Handler.GoRestfulContainer.Add(ws)
}

func (h *Handler) registerMachine(req *restful.Request, resp *restful.Response) {
	var body struct {
		Name            string                           `json:"name"`
		ClientSecret    string                           `json:"clientSecret"`
		TokenTTLSeconds int64                            `json:"tokenTTLSeconds"`
		Type            json.RawMessage                  `json:"type"`
		Projects        []string                         `json:"projects"`
		Permissions     []iamv1.MachineAccountPermission `json:"permissions"`
	}
	if err := decode(req.Request, &body); err != nil || body.Name == "" || len(utilvalidation.IsDNS1123Label(body.Name)) != 0 {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}
	if body.TokenTTLSeconds == 0 {
		body.TokenTTLSeconds = 3600
	}
	accountType := iamv1.MachineAccountRunner
	if len(body.Type) != 0 {
		accountType = ""
		if err := json.Unmarshal(body.Type, &accountType); err != nil || !accountType.IsValid() {
			writeError(resp, http.StatusBadRequest, "type must be runner or service")
			return
		}
	}
	account := &iamv1.MachineAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: iamv1.SchemeGroupVersion.String(), Kind: "MachineAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: body.Name},
		Spec: iamv1.MachineAccountSpec{
			Type: accountType, TokenTTLSeconds: body.TokenTTLSeconds,
			Projects: body.Projects, Permissions: body.Permissions,
		},
	}
	if errs := iamvalidation.ValidateMachineAccount(account); len(errs) != 0 {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}
	credentialData, err := credential.NewMachineCredential(body.ClientSecret)
	if err != nil {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}
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
	var body struct {
		Name         string `json:"name"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := decode(req.Request, &body); err != nil || body.Name == "" || body.ClientSecret == "" {
		writeError(resp, http.StatusBadRequest, "invalid request")
		return
	}
	spec, ok, err := h.credentials.AuthenticateMachine(req.Request.Context(), body.Name, body.ClientSecret)
	if err != nil {
		writeError(resp, http.StatusInternalServerError, "authentication unavailable")
		return
	}
	if !ok {
		writeError(resp, http.StatusUnauthorized, "authentication failed")
		return
	}
	_ = resp.WriteHeaderAndEntity(http.StatusOK, map[string]interface{}{
		"authenticated": true, "name": body.Name, "type": spec.Type, "tokenTTLSeconds": spec.TokenTTLSeconds,
	})
}

func (h *Handler) register(req *restful.Request, resp *restful.Response) {
	body, err := decodeUserRequest(req.Request, true)
	if err != nil {
		writeOAuthError(resp, invalidRequest())
		return
	}
	if body.Method == "oauth" {
		h.registerOAuth(req, resp, body)
		return
	}
	if !validRegistration(body.Username, body.Password, body.Email) {
		writeError(resp, http.StatusBadRequest, "invalid registration request")
		return
	}

	enabled := true
	scope := "ebs:user"
	if body.Username == "admin" {
		scope = "ebs:admin"
	}
	user := &iamv1.User{
		TypeMeta:   metav1.TypeMeta{APIVersion: iamv1.SchemeGroupVersion.String(), Kind: "User"},
		ObjectMeta: metav1.ObjectMeta{Name: body.Username},
		Spec:       iamv1.UserSpec{Enabled: &enabled, Scopes: []string{scope}, DisplayName: body.DisplayName, Email: body.Email},
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
	resp.AddHeader("Cache-Control", "no-store")
	body, err := decodeUserRequest(req.Request, false)
	if err != nil {
		writeOAuthError(resp, invalidRequest())
		return
	}
	if body.Method == "oauth" {
		h.authenticateOAuth(req, resp, body)
		return
	}
	if body.Method == "credential" {
		h.authenticatePersonalCredential(req, resp, body.Token)
		return
	}
	if body.Username == "" || body.Password == "" {
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

func requireGateway(req *restful.Request, resp *restful.Response) bool {
	principal, ok := genericapirequest.UserFrom(req.Request.Context())
	if !ok || principal.GetName() == "system:anonymous" {
		_ = resp.WriteHeaderAndEntity(http.StatusUnauthorized, map[string]string{"code": "Unauthorized", "error": "internal authentication required"})
		return false
	}
	if principal.GetName() != "spiffe://eulermaker/internal/ebs-gateway" {
		_ = resp.WriteHeaderAndEntity(http.StatusForbidden, map[string]string{"code": "Forbidden", "error": "Gateway identity required"})
		return false
	}
	return true
}

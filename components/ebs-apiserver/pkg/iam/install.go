package iam

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"time"

	"github.com/emicklei/go-restful/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
	"ebs-apiserver/pkg/iam/credential"
)

const maxRequestBody = 1 << 20

const (
	registrationPendingAnnotation = "iam.ebs.io/registration-pending"
	registrationStartedAnnotation = "iam.ebs.io/registration-started-at"
)

type userStorage interface {
	Create(context.Context, runtime.Object, rest.ValidateObjectFunc, *metav1.CreateOptions) (runtime.Object, error)
	Get(context.Context, string, *metav1.GetOptions) (runtime.Object, error)
	Update(context.Context, string, rest.UpdatedObjectInfo, rest.ValidateObjectFunc, rest.ValidateObjectUpdateFunc, bool, *metav1.UpdateOptions) (runtime.Object, bool, error)
	Delete(context.Context, string, rest.ValidateObjectFunc, *metav1.DeleteOptions) (runtime.Object, bool, error)
}

type Handler struct {
	credentials *credential.Store
	users       userStorage
}

func InstallInternalRoutes(server *genericapiserver.GenericAPIServer, credentials *credential.Store, users userStorage) {
	h := &Handler{credentials: credentials, users: users}
	ws := new(restful.WebService).Path("/internal/iam/v1").Consumes(restful.MIME_JSON).Produces(restful.MIME_JSON)
	ws.Route(ws.POST("/register").To(h.register))
	ws.Route(ws.POST("/authenticate").To(h.authenticate))
	ws.Route(ws.PUT("/users/{name}/password").To(h.setPassword))
	server.Handler.GoRestfulContainer.Add(ws)
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

	disabled := false
	user := &iamv1.User{
		TypeMeta: metav1.TypeMeta{APIVersion: iamv1.SchemeGroupVersion.String(), Kind: "User"},
		ObjectMeta: metav1.ObjectMeta{
			Name: body.Username,
			Annotations: map[string]string{
				registrationPendingAnnotation: "true",
				registrationStartedAnnotation: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
		Spec: iamv1.UserSpec{Enabled: &disabled, DisplayName: body.DisplayName, Email: body.Email},
	}
	created, err := h.createRegistrationUser(req.Request.Context(), user)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeError(resp, http.StatusConflict, "username already exists")
			return
		}
		writeError(resp, http.StatusInternalServerError, "registration unavailable")
		return
	}
	createdUser := created.(*iamv1.User)

	if err := h.credentials.SetPassword(req.Request.Context(), body.Username, body.Password); err != nil {
		h.compensateRegistration(req.Request.Context(), createdUser)
		if errors.Is(err, credential.ErrInvalidPassword) {
			writeError(resp, http.StatusBadRequest, "invalid registration request")
			return
		}
		writeError(resp, http.StatusInternalServerError, "registration unavailable")
		return
	}

	enabled := true
	createdUser.Spec.Enabled = &enabled
	delete(createdUser.Annotations, registrationPendingAnnotation)
	delete(createdUser.Annotations, registrationStartedAnnotation)
	if len(createdUser.Annotations) == 0 {
		createdUser.Annotations = nil
	}
	if _, _, err := h.users.Update(req.Request.Context(), body.Username, rest.DefaultUpdatedObjectInfo(createdUser), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		h.compensateRegistration(req.Request.Context(), createdUser)
		writeError(resp, http.StatusInternalServerError, "registration unavailable")
		return
	}

	_ = resp.WriteHeaderAndEntity(http.StatusCreated, map[string]string{"username": body.Username})
}

func (h *Handler) createRegistrationUser(ctx context.Context, user *iamv1.User) (runtime.Object, error) {
	created, err := h.users.Create(ctx, user, nil, &metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return created, err
	}
	existingObject, getErr := h.users.Get(ctx, user.Name, &metav1.GetOptions{})
	if getErr != nil {
		return nil, err
	}
	existing := existingObject.(*iamv1.User)
	startedAt, parseErr := time.Parse(time.RFC3339Nano, existing.Annotations[registrationStartedAnnotation])
	if existing.Annotations[registrationPendingAnnotation] != "true" || parseErr != nil || time.Since(startedAt) < 10*time.Minute {
		return nil, err
	}
	if !h.compensateRegistration(ctx, existing) {
		return nil, err
	}
	return h.users.Create(ctx, user, nil, &metav1.CreateOptions{})
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

func (h *Handler) compensateRegistration(ctx context.Context, user *iamv1.User) bool {
	rv := user.ResourceVersion
	_, deleted, err := h.users.Delete(ctx, user.Name, nil, &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: &rv}})
	if err != nil || !deleted {
		return false
	}
	_ = h.credentials.Delete(ctx, user.Name)
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

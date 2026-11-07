package iam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emicklei/go-restful/v3"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	request "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/klog/v2"

	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/iam/credential"
	"ebs-apiserver/pkg/storage/es"
)

type oauthIdentity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type userRequest struct {
	Method, Username, Password, DisplayName, Email string
	Identity                                       oauthIdentity
}

// decodeUserRequest validates method-specific fields before any identity lookup or reservation.
func decodeUserRequest(req *http.Request, registration bool) (userRequest, error) {
	defer req.Body.Close()
	var body userRequest
	data, err := io.ReadAll(io.LimitReader(req.Body, maxRequestBody+1))
	if err != nil || len(data) > maxRequestBody || !utf8.Valid(data) {
		return body, invalidRequest()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil || !validJSONUnicode(data) {
		return body, invalidRequest()
	}
	body.Method = "password"
	if raw, exists := fields["method"]; exists {
		if string(raw) == "null" || json.Unmarshal(raw, &body.Method) != nil {
			return body, invalidRequest()
		}
	}
	if body.Method != "password" && body.Method != "oauth" {
		return body, invalidRequest()
	}
	allowed := map[string]*string{"method": &body.Method}
	if registration || body.Method == "password" {
		allowed["username"] = &body.Username
	}
	if body.Method == "password" {
		allowed["password"] = &body.Password
	}
	if registration {
		allowed["displayName"], allowed["email"] = &body.DisplayName, &body.Email
	}
	for name, raw := range fields {
		if name == "externalIdentity" && body.Method == "oauth" {
			if bytes.Equal(raw, []byte("null")) {
				return body, invalidRequest()
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&body.Identity) != nil {
				return body, invalidRequest()
			}
			continue
		}
		target, ok := allowed[name]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, target) != nil {
			return body, invalidRequest()
		}
	}
	if body.Method == "oauth" {
		if !validIdentity(body.Identity) || (registration && !validOAuthProfile(body)) {
			return body, invalidRequest()
		}
	}
	return body, nil
}

// validJSONUnicode rejects unpaired UTF-16 escapes in syntactically valid JSON.
// encoding/json would replace them with U+FFFD, changing the identity used for uniqueness.
func validJSONUnicode(data []byte) bool {
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if data[i] != 'u' {
			continue
		}
		value, _ := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		i += 4
		if value >= 0xD800 && value <= 0xDBFF {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 6
		} else if value >= 0xDC00 && value <= 0xDFFF {
			return false
		}
	}
	return true
}

func validIdentity(identity oauthIdentity) bool {
	valid := func(value string, limit int) bool {
		return value != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
	}
	return valid(identity.Issuer, 2048) && valid(identity.Subject, 1024)
}

func validOAuthProfile(body userRequest) bool {
	if len(utilvalidation.IsDNS1123Label(body.Username)) != 0 || !utf8.ValidString(body.DisplayName) {
		return false
	}
	if body.Email != "" {
		address, err := mail.ParseAddress(body.Email)
		return err == nil && address.Address == body.Email
	}
	return true
}

func identityKey(identity oauthIdentity) string {
	hash := sha256.New()
	for _, value := range []string{identity.Issuer, identity.Subject} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

type oauthError struct {
	status  int
	code    string
	message string
}

func (e *oauthError) Error() string { return e.code }
func invalidRequest() *oauthError   { return &oauthError{400, "InvalidRequest", "invalid request"} }
func unavailable() *oauthError {
	return &oauthError{503, "IAMUnavailable", "identity service temporarily unavailable"}
}
func inProgress() *oauthError {
	return &oauthError{409, "RegistrationInProgress", "registration requires confirmation"}
}
func invalidState() *oauthError {
	return &oauthError{500, "IdentityStateInvalid", "identity state requires maintenance"}
}

func writeOAuthError(resp *restful.Response, err error) {
	var failure *oauthError
	if !errors.As(err, &failure) {
		failure = unavailable()
	}
	if failure.status >= 500 || failure.code == "RegistrationInProgress" {
		// Do not log identity data, credentials, or raw Elasticsearch responses.
		klog.ErrorS(nil, "OAuth IAM operation failed", "code", failure.code)
	}
	_ = resp.WriteHeaderAndEntity(failure.status, map[string]string{"code": failure.code, "error": failure.message})
}

func oauthGateway(req *restful.Request, resp *restful.Response) bool {
	principal, ok := request.UserFrom(req.Request.Context())
	if !ok || principal.GetName() == "system:anonymous" {
		writeOAuthError(resp, &oauthError{401, "Unauthorized", "internal authentication required"})
		return false
	}
	if principal.GetName() != "spiffe://eulermaker/internal/ebs-gateway" {
		writeOAuthError(resp, &oauthError{403, "Forbidden", "Gateway identity required"})
		return false
	}
	return true
}

func (h *Handler) authenticateOAuth(req *restful.Request, resp *restful.Response, body userRequest) {
	if !oauthGateway(req, resp) {
		return
	}
	claim, err := h.oauth.lookup(req.Request.Context(), body.Identity)
	if err != nil {
		writeOAuthError(resp, err)
		return
	}
	if claim == nil {
		_ = resp.WriteHeaderAndEntity(200, map[string]bool{"authenticated": false, "registrationRequired": true})
		return
	}
	user, err := h.oauth.resolve(req.Request.Context(), claim)
	if err != nil {
		writeOAuthError(resp, err)
		return
	}
	if !credential.CanAuthenticate(user) {
		writeOAuthError(resp, &oauthError{401, "AuthenticationFailed", "authentication failed"})
		return
	}
	_ = resp.WriteHeaderAndEntity(200, map[string]interface{}{"authenticated": true, "username": user.Name, "uid": user.UID})
}

func (h *Handler) registerOAuth(req *restful.Request, resp *restful.Response, body userRequest) {
	if !oauthGateway(req, resp) {
		return
	}
	user, created, err := h.oauth.register(req.Request.Context(), body)
	if err != nil {
		writeOAuthError(resp, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	_ = resp.WriteHeaderAndEntity(status, map[string]string{"username": user.Name, "uid": string(user.UID)})
}

type oauthUserStorage interface {
	CreateWithExternalIdentity(context.Context, runtime.Object, *es.ExternalIdentity) (runtime.Object, error)
}

type oauthService struct {
	client *es.Client
	users  oauthUserStorage
}

func (s *oauthService) lookup(ctx context.Context, identity oauthIdentity) (*es.OAuthIdentityHit, error) {
	hit, err := s.client.GetOAuthIdentity(ctx, identityKey(identity))
	if es.IsDocumentNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, unavailable()
	}
	claim := hit.Claim
	_, uidErr := uuid.Parse(claim.UserUID)
	_, operationErr := uuid.Parse(claim.OperationID)
	if claim.Issuer != identity.Issuer || claim.Subject != identity.Subject || uidErr != nil || operationErr != nil || len(utilvalidation.IsDNS1123Label(claim.UserName)) != 0 {
		return nil, invalidState()
	}
	return hit, nil
}

func (s *oauthService) resolve(ctx context.Context, claim *es.OAuthIdentityHit) (*iamv1.User, error) {
	hit, err := s.client.Get(ctx, "user", claim.Claim.UserName)
	if es.IsDocumentNotFound(err) {
		return nil, inProgress()
	}
	if err != nil {
		return nil, unavailable()
	}
	var user iamv1.User
	identity := hit.Document.ExternalIdentity
	if json.Unmarshal(hit.Document.Data, &user) != nil || user.Name != claim.Claim.UserName || string(user.UID) != claim.Claim.UserUID || identity == nil || identity.Issuer != claim.Claim.Issuer || identity.Subject != claim.Claim.Subject {
		return nil, invalidState()
	}
	return &user, nil
}

func (s *oauthService) confirm(ctx context.Context, body userRequest) (*iamv1.User, bool, error) {
	claim, err := s.lookup(ctx, body.Identity)
	if err != nil {
		return nil, false, err
	}
	if claim == nil {
		return nil, false, unavailable()
	}
	return s.existing(ctx, claim, body.Username)
}

func (s *oauthService) existing(ctx context.Context, claim *es.OAuthIdentityHit, username string) (*iamv1.User, bool, error) {
	user, err := s.resolve(ctx, claim)
	if err != nil {
		return nil, false, err
	}
	if user.Name != username {
		return nil, false, &oauthError{409, "IdentityAlreadyLinked", "identity already belongs to an account"}
	}
	if !credential.CanAuthenticate(user) {
		return nil, false, &oauthError{403, "AccountUnavailable", "account is unavailable"}
	}
	return user, false, nil
}

func (s *oauthService) register(ctx context.Context, body userRequest) (*iamv1.User, bool, error) {
	claim, err := s.lookup(ctx, body.Identity)
	if err != nil {
		return nil, false, err
	}
	if claim != nil {
		return s.existing(ctx, claim, body.Username)
	}
	key := identityKey(body.Identity)
	reserved := es.OAuthIdentityClaim{
		Issuer: body.Identity.Issuer, Subject: body.Identity.Subject,
		UserName: body.Username, UserUID: uuid.NewString(), OperationID: uuid.NewString(),
	}
	version, err := s.client.CreateOAuthIdentity(ctx, key, reserved)
	if err != nil {
		return s.confirm(ctx, body)
	}
	enabled := true
	user := &iamv1.User{
		TypeMeta:   metav1.TypeMeta{APIVersion: iamv1.SchemeGroupVersion.String(), Kind: "User"},
		ObjectMeta: metav1.ObjectMeta{Name: body.Username, UID: types.UID(reserved.UserUID)},
		Spec:       iamv1.UserSpec{Enabled: &enabled, Scopes: []string{"ebs:user"}, DisplayName: body.DisplayName, Email: body.Email},
	}
	identity := &es.ExternalIdentity{Issuer: reserved.Issuer, Subject: reserved.Subject, LinkedAt: time.Now().UTC().Format(time.RFC3339)}
	_, err = s.users.CreateWithExternalIdentity(request.WithNamespace(ctx, ""), user, identity)
	if err == nil {
		return user, true, nil
	}
	if apierrors.IsAlreadyExists(err) {
		return nil, false, s.releaseRejected(ctx, key, reserved, version)
	}
	return s.confirm(ctx, body)
}

// releaseRejected is only called by the writer after a definitive create-only username conflict.
func (s *oauthService) releaseRejected(ctx context.Context, key string, reserved es.OAuthIdentityClaim, version es.Version) error {
	user, err := s.client.Get(ctx, "user", reserved.UserName)
	if err != nil {
		return unavailable()
	}
	var obj iamv1.User
	if json.Unmarshal(user.Document.Data, &obj) != nil || obj.UID == "" || obj.Name != reserved.UserName || string(obj.UID) == reserved.UserUID {
		return invalidState()
	}
	if identity := user.Document.ExternalIdentity; identity != nil && identity.Issuer == reserved.Issuer && identity.Subject == reserved.Subject {
		return invalidState()
	}
	current, err := s.client.GetOAuthIdentity(ctx, key)
	if err != nil || current.Claim != reserved || current.Version != version {
		return unavailable()
	}
	if err := s.client.DeleteOAuthIdentity(ctx, key, version); err != nil {
		return unavailable()
	}
	return &oauthError{409, "UsernameAlreadyExists", "username already exists"}
}

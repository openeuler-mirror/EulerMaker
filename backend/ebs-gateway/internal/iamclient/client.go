package iamclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"k8s.io/apimachinery/pkg/util/sets"
	validation "k8s.io/apimachinery/pkg/util/validation"

	iamv1 "ebs-api/iam/v1"

	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/mutation"
	"ebs-gateway/internal/upstream"
)

type Client struct {
	upstream *upstream.Client
}

func New(upstreamClient *upstream.Client) *Client {
	return &Client{upstream: upstreamClient}
}

type User struct {
	Name    string
	UID     string
	Enabled bool
	Scopes  []identity.Scope
}

func (c *Client) GetUser(ctx context.Context, name string) (User, int, error) {
	response, err := c.upstream.Do(ctx, http.MethodGet, "/apis/iam.ebs/v1/users/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return User{}, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return User{}, response.StatusCode, nil
	}
	var object iamv1.User
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&object); err != nil {
		return User{}, response.StatusCode, fmt.Errorf("decode IAM user: %w", err)
	}
	scopes := make([]identity.Scope, len(object.Spec.Scopes))
	for i, scope := range object.Spec.Scopes {
		scopes[i] = identity.Scope(scope)
	}
	return User{
		Name:    object.Name,
		UID:     string(object.UID),
		Enabled: object.Spec.Enabled != nil && *object.Spec.Enabled,
		Scopes:  scopes,
	}, response.StatusCode, nil
}

func (c *Client) AuthenticateUser(ctx context.Context, name, password string) (bool, int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/users/authenticate", map[string]string{"username": name, "password": password})
	if err != nil {
		return false, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, response.StatusCode, nil
	}
	var result struct {
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return false, response.StatusCode, fmt.Errorf("decode authentication response: %w", err)
	}
	return result.Authenticated && result.Username == name, response.StatusCode, nil
}

func (c *Client) RegisterUser(ctx context.Context, payload any) (int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/users/register", payload)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (c *Client) SetPassword(ctx context.Context, name, password string) (int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPut, "/internal/iam/v1/users/"+url.PathEscape(name)+"/password", map[string]string{"password": password})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

type ExternalIdentity struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

type OAuthAccount struct {
	Authenticated        bool   `json:"authenticated"`
	RegistrationRequired bool   `json:"registrationRequired"`
	Username             string `json:"username"`
	UID                  string `json:"uid"`
}

// OAuthError contains only a recognized IAM code, never an upstream diagnostic.
type OAuthError struct {
	Status int
	Code   string
}

func (e *OAuthError) Error() string { return e.Code }

func (c *Client) AuthenticateOAuth(ctx context.Context, identity ExternalIdentity) (OAuthAccount, error) {
	result, err := c.oauthRequest(ctx, "/internal/iam/v1/users/authenticate", map[string]any{
		"method": "oauth", "externalIdentity": identity,
	}, false)
	if err == nil && !((result.Authenticated && !result.RegistrationRequired && result.Username != "" && result.UID != "") ||
		(!result.Authenticated && result.RegistrationRequired && result.Username == "" && result.UID == "")) {
		err = &OAuthError{503, "OAuthUnavailable"}
	}
	return result, err
}

func (c *Client) RegisterOAuth(ctx context.Context, identity ExternalIdentity, username, displayName, email string) (OAuthAccount, error) {
	return c.oauthRequest(ctx, "/internal/iam/v1/users/register", map[string]any{
		"method": "oauth", "externalIdentity": identity, "username": username, "displayName": displayName, "email": email,
	}, true)
}

type MachineAuthentication struct {
	Name string `json:"name"`
	iamv1.MachineAccountSpec
}

func (c *Client) GetMachine(ctx context.Context, name string) (*iamv1.MachineAccount, int, error) {
	response, err := c.upstream.Do(ctx, http.MethodGet, "/apis/iam.ebs/v1/machineaccounts/"+url.PathEscape(name), nil, nil)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, response.StatusCode, nil
	}
	var account iamv1.MachineAccount
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&account); err != nil {
		return nil, response.StatusCode, fmt.Errorf("decode IAM machine account: %w", err)
	}
	if account.Name == "" || !account.Spec.Type.IsValid() {
		return nil, response.StatusCode, fmt.Errorf("invalid IAM machine account response")
	}
	projects := sets.New[string]()
	for _, project := range account.Spec.Projects {
		if len(validation.IsDNS1123Label(project)) != 0 || projects.Has(project) {
			return nil, response.StatusCode, fmt.Errorf("invalid IAM machine account projects")
		}
		projects.Insert(project)
	}
	permissions := sets.New[iamv1.MachineAccountPermission]()
	for _, permission := range account.Spec.Permissions {
		if !permission.IsValid() || permissions.Has(permission) {
			return nil, response.StatusCode, fmt.Errorf("invalid IAM machine account permissions")
		}
		permissions.Insert(permission)
	}
	return &account, response.StatusCode, nil
}

func (c *Client) AuthenticateMachine(ctx context.Context, name, secret string) (MachineAuthentication, bool, int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/machineaccounts/authenticate", map[string]string{"name": name, "clientSecret": secret})
	if err != nil {
		return MachineAuthentication{}, false, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return MachineAuthentication{}, false, response.StatusCode, nil
	}
	var result struct {
		Authenticated bool `json:"authenticated"`
		MachineAuthentication
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return MachineAuthentication{}, false, response.StatusCode, fmt.Errorf("decode machine authentication response: %w", err)
	}
	if !result.Type.IsValid() {
		return MachineAuthentication{}, false, response.StatusCode, fmt.Errorf("invalid machine account type in authentication response")
	}
	return result.MachineAuthentication, result.Authenticated && result.Name == name, response.StatusCode, nil
}

func (c *Client) RegisterMachine(ctx context.Context, payload any) (int, error) {
	response, err := c.jsonRequest(ctx, http.MethodPost, "/internal/iam/v1/machineaccounts/register", payload)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func (c *Client) oauthRequest(ctx context.Context, path string, input any, registration bool) (OAuthAccount, error) {
	unavailable := &OAuthError{503, "OAuthUnavailable"}
	response, err := c.jsonRequest(ctx, http.MethodPost, path, input)
	if err != nil {
		return OAuthAccount{}, unavailable
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return OAuthAccount{}, unavailable
	}
	object, err := mutation.ParseObject(data)
	if err != nil {
		return OAuthAccount{}, unavailable
	}
	if response.StatusCode != 200 && !(registration && response.StatusCode == 201) {
		var failure struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(data, &failure) != nil {
			return OAuthAccount{}, unavailable
		}
		statuses := map[string]int{
			"InvalidRequest": 400, "UsernameAlreadyExists": 409, "RegistrationInProgress": 409,
			"IdentityAlreadyLinked": 409, "IdentityStateInvalid": 500, "IAMUnavailable": 503,
			"AuthenticationFailed": 401, "AccountUnavailable": 403,
		}
		if status, ok := statuses[failure.Code]; ok && status == response.StatusCode {
			return OAuthAccount{}, &OAuthError{status, failure.Code}
		}
		return OAuthAccount{}, unavailable
	}
	var result OAuthAccount
	if !registration {
		if _, ok := object["authenticated"].(bool); !ok {
			return OAuthAccount{}, unavailable
		}
		if value, exists := object["registrationRequired"]; exists {
			if _, ok := value.(bool); !ok {
				return OAuthAccount{}, unavailable
			}
		}
	}
	if json.Unmarshal(data, &result) != nil || (registration && (result.Username == "" || result.UID == "")) {
		return OAuthAccount{}, unavailable
	}
	return result, nil
}

func (c *Client) jsonRequest(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return c.upstream.Do(ctx, method, path, bytes.NewReader(body), http.Header{"Content-Type": []string{"application/json"}})
}

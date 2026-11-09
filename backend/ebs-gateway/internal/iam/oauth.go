package iam

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"ebs-gateway/internal/mutation"
)

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

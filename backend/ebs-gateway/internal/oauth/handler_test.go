package oauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	iamv1 "ebs-api/iam/v1"

	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

func TestLocalAuthenticationEnabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config Config
		want   bool
	}{
		{"local", Config{}, true},
		{"OAuth", testConfig("https://id.example.com"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(tc.config, nil, nil, nil)
			if got := h.LocalAuthenticationEnabled(); got != tc.want {
				t.Fatalf("LocalAuthenticationEnabled()=%v, want %v", got, tc.want)
			}
		})
	}
}

type fakeAccounts struct {
	account       iamclient.OAuthResult
	user          iamv1.User
	registerCalls int
	authCalls     int
	registerError *iamclient.OAuthError
	authError     *iamclient.OAuthError
}

func (f *fakeAccounts) RoundTrip(r *http.Request) (*http.Response, error) {
	response := httptest.NewRecorder()
	response.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == "/apis/iam.ebs/v1/users/"+f.user.Name {
		_ = json.NewEncoder(response).Encode(f.user)
		return response.Result(), nil
	}
	if r.Method != http.MethodPost {
		return nil, fmt.Errorf("unexpected IAM request: %s %s", r.Method, r.URL.Path)
	}
	var input struct {
		Method           string                     `json:"method"`
		Username         string                     `json:"username"`
		ExternalIdentity iamclient.ExternalIdentity `json:"externalIdentity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return nil, err
	}
	if input.Method != "oauth" || input.ExternalIdentity.Issuer == "" || input.ExternalIdentity.Subject == "" {
		return nil, fmt.Errorf("missing OAuth identity in IAM request")
	}
	var failure *iamclient.OAuthError
	result := f.account
	switch r.URL.Path {
	case "/internal/iam/v1/users/authenticate":
		f.authCalls++
		failure = f.authError
		if result.Username == "" {
			result = iamclient.OAuthResult{RegistrationRequired: true}
		}
	case "/internal/iam/v1/users/register":
		f.registerCalls++
		failure = f.registerError
		if failure == nil {
			f.account = iamclient.OAuthResult{Authenticated: true, Username: input.Username, UID: "user-uid"}
			f.user = iamv1.User{
				ObjectMeta: metav1.ObjectMeta{Name: input.Username, UID: "user-uid"},
				Spec:       iamv1.UserSpec{Enabled: ptr.To(true), Scopes: []string{string(identity.UserScope)}},
			}
			result = f.account
			response.WriteHeader(http.StatusCreated)
		}
	default:
		return nil, fmt.Errorf("unexpected IAM path: %s", r.URL.Path)
	}
	if failure != nil {
		response.WriteHeader(failure.Status)
		_ = json.NewEncoder(response).Encode(map[string]string{"code": failure.Code})
	} else {
		_ = json.NewEncoder(response).Encode(result)
	}
	return response.Result(), nil
}

type oauthFixture struct {
	h              *Handler
	accounts       *fakeAccounts
	server         *httptest.Server
	now            time.Time
	authorization  url.Values
	changeClaims   func(map[string]any)
	userSub        string
	tokenCalls     atomic.Int32
	keyCalls       atomic.Int32
	requestInvalid atomic.Bool
}

func newFixture(t *testing.T) *oauthFixture {
	t.Helper()
	f := &oauthFixture{accounts: &fakeAccounts{}, now: time.Now().UTC(), userSub: "external-id"}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "key-1"))
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token",
				"jwks_uri": f.server.URL + "/keys", "userinfo_endpoint": f.server.URL + "/user",
				"code_challenge_methods_supported": []string{"S256"}, "id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/keys":
			f.keyCalls.Add(1)
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "key-1", Algorithm: "RS256", Use: "sig"}}})
		case "/token":
			f.tokenCalls.Add(1)
			r.ParseForm()
			client, secret, ok := r.BasicAuth()
			if !ok || client != "client" || secret != "secret" || r.Form.Get("code") != "code" ||
				r.Form.Get("redirect_uri") != "https://ebs.example.com/auth/callback" || digest(r.Form.Get("code_verifier")) != f.authorization.Get("code_challenge") {
				f.requestInvalid.Store(true)
				w.WriteHeader(400)
				return
			}
			claims := map[string]any{"iss": f.server.URL, "sub": "external-id", "aud": "client", "iat": f.now.Unix(), "exp": f.now.Add(time.Minute).Unix(), "nonce": f.authorization.Get("nonce")}
			if f.changeClaims != nil {
				f.changeClaims(claims)
			}
			payload, _ := json.Marshal(claims)
			signed, err := signer.Sign(payload)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			id, _ := signed.CompactSerialize()
			json.NewEncoder(w).Encode(map[string]string{"id_token": id, "access_token": "access-token", "token_type": "Bearer"})
		case "/user":
			if r.Header.Get("Authorization") != "Bearer access-token" {
				f.requestInvalid.Store(true)
				w.WriteHeader(401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"sub": f.userSub, "openeuler_username": "community-alice", "name": "Alice", "email": "alice@example.com", "email_verified": true})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	config := testConfig(f.server.URL)
	tokens, _ := identity.NewTokens(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	client, err := upstream.New("https://api.example", f.accounts)
	if err != nil {
		t.Fatal(err)
	}
	f.h = New(config, iamclient.New(client), tokens, func() time.Time { return f.now })
	f.h.provider = newProvider(config, f.h.now, f.server.Client().Transport)
	return f
}

func TestLoginInitializesProviderOnDemand(t *testing.T) {
	f := newFixture(t)
	base := f.h.provider.client.Transport
	calls, available := 0, false
	f.h.provider.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !available {
			return nil, fmt.Errorf("provider offline")
		}
		return base.RoundTrip(r)
	})
	if f.h.provider.ready {
		t.Fatal("provider initialized before a login request")
	}
	if response := serve(f.h, http.MethodGet, "/auth/providers", ""); response.Code != http.StatusOK || calls != 0 {
		t.Fatalf("provider listing triggered initialization: status=%d calls=%d", response.Code, calls)
	}
	for _, tc := range []struct {
		name      string
		advance   time.Duration
		available bool
		status    int
		calls     int
	}{
		{"initial failure", 0, false, http.StatusServiceUnavailable, 1},
		{"retry cooldown", 0, true, http.StatusServiceUnavailable, 1},
		{"recovery", 30 * time.Second, true, http.StatusFound, 2},
		{"cached configuration", time.Minute, true, http.StatusFound, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.now = f.now.Add(tc.advance)
			available = tc.available
			response := serve(f.h, http.MethodGet, "/auth/oauth/community/login", "")
			if response.Code != tc.status || calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, calls, response.Body)
			}
		})
	}
}

func serve(h *Handler, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/auth/providers", h.Providers)
	router.GET("/auth/oauth/:provider/login", h.Login)
	router.POST("/auth/oauth/:provider/callback", h.Callback)
	router.POST("/auth/oauth/register", h.Register)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", h.config.PublicBaseURL)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	result := httptest.NewRecorder()
	router.ServeHTTP(result, req)
	return result
}

func cookieFrom(t *testing.T, result *httptest.ResponseRecorder, purpose string) *http.Cookie {
	t.Helper()
	for _, c := range result.Result().Cookies() {
		if c.Name == cookieName(purpose) && c.MaxAge > 0 {
			return c
		}
	}
	t.Fatalf("missing %s cookie: %d %s", purpose, result.Code, result.Body.String())
	return nil
}

func (f *oauthFixture) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	response := serve(f.h, "GET", "/auth/oauth/community/login?returnTo=%2Fprojects%3Ftab%3Dbuilds", "")
	if response.Code != 302 {
		t.Fatalf("login: %d %s", response.Code, response.Body.String())
	}
	location, _ := url.Parse(response.Header().Get("Location"))
	f.authorization = location.Query()
	if f.authorization.Get("code_challenge_method") != "S256" || f.authorization.Get("response_type") != "code" {
		t.Fatal(f.authorization)
	}
	cookie := cookieFrom(t, response, "login")
	if !cookie.HttpOnly || !cookie.Secure || cookie.Domain != "" || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	return cookie, f.authorization.Get("state")
}

func (f *oauthFixture) callback(t *testing.T) *httptest.ResponseRecorder {
	cookie, state := f.login(t)
	body, _ := json.Marshal(map[string]string{"code": "code", "state": state})
	return serve(f.h, "POST", "/auth/oauth/community/callback", string(body), cookie)
}

func TestLoginExistingAccount(t *testing.T) {
	f := newFixture(t)
	f.accounts.account = iamclient.OAuthResult{Authenticated: true, Username: "alice", UID: "user-uid"}
	f.accounts.user = iamv1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "user-uid"},
		Spec:       iamv1.UserSpec{Enabled: ptr.To(true), Scopes: []string{string(identity.UserScope)}},
	}
	result := f.callback(t)
	assertToken(t, f, result, "alice")
	if f.tokenCalls.Load() != 1 || f.keyCalls.Load() != 1 || f.requestInvalid.Load() {
		t.Fatal("unexpected provider exchange")
	}
}

func assertToken(t *testing.T, f *oauthFixture, result *httptest.ResponseRecorder, name string) {
	t.Helper()
	var output struct {
		Token    string
		ReturnTo string
	}
	json.Unmarshal(result.Body.Bytes(), &output)
	if result.Code != 200 || output.Token == "" || output.ReturnTo != "/projects?tab=builds" {
		t.Fatalf("login result: %d %s", result.Code, result.Body.String())
	}
	principal, err := f.h.tokens.Verify(output.Token, f.now)
	if err != nil || principal.Subject != name || principal.Scope != identity.UserScope {
		t.Fatalf("principal=%+v err=%v", principal, err)
	}
	if result.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}
}

func TestRegistrationAcrossReplicasAndConfirmation(t *testing.T) {
	f := newFixture(t)
	result := f.callback(t)
	var form struct {
		RegistrationID       string
		RegistrationRequired bool
		ExpiresAt            time.Time
		Profile              struct {
			Username    string
			DisplayName string
			Email       string
		}
	}
	if err := json.Unmarshal(result.Body.Bytes(), &form); err != nil {
		t.Fatal(err)
	}
	if result.Code != 200 || !form.RegistrationRequired || f.accounts.registerCalls != 0 {
		t.Fatalf("%d %s", result.Code, result.Body.String())
	}
	if form.RegistrationID == "" || !form.ExpiresAt.Equal(f.now.Add(contextTTL).Truncate(time.Second)) ||
		form.Profile.Username != "community-alice" || form.Profile.DisplayName != "Alice" || form.Profile.Email != "alice@example.com" {
		t.Fatalf("callback must provide the registration form context: %s", result.Body.String())
	}
	cookie := cookieFrom(t, result, "register")
	other := New(f.h.config, f.h.accounts, f.h.tokens, f.h.now)
	body, _ := json.Marshal(map[string]string{"registrationID": form.RegistrationID, "action": "create", "username": "admin"})
	result = serve(other, "POST", "/auth/oauth/register", string(body), cookie)
	assertToken(t, f, result, "admin")
	if f.accounts.registerCalls != 1 {
		t.Fatal("registration not executed once")
	}
	body, _ = json.Marshal(map[string]string{"registrationID": form.RegistrationID, "action": "confirm", "username": "admin"})
	result = serve(f.h, "POST", "/auth/oauth/register", string(body), cookie)
	assertToken(t, f, result, "admin")
	if f.accounts.registerCalls != 1 {
		t.Fatal("confirmation wrote IAM")
	}
	for _, enabled := range []*bool{ptr.To(false), nil} {
		f.accounts.user.Spec.Enabled = enabled
		if result = serve(other, "POST", "/auth/oauth/register", string(body), cookie); result.Code != 401 {
			t.Fatal(result.Body.String())
		}
	}
}

func TestRegistrationUnknownResultOnlyConfirms(t *testing.T) {
	f := newFixture(t)
	result := f.callback(t)
	cookie := cookieFrom(t, result, "register")
	var form struct{ RegistrationID string }
	json.Unmarshal(result.Body.Bytes(), &form)
	f.accounts.registerError = &iamclient.OAuthError{Status: 503, Code: "IAMUnavailable"}
	body := `{"registrationID":"` + form.RegistrationID + `","action":"create","username":"alice"}`
	if result = serve(f.h, "POST", "/auth/oauth/register", body, cookie); result.Code != 503 {
		t.Fatal(result.Body.String())
	}
	body = strings.Replace(body, `"create"`, `"confirm"`, 1)
	if result = serve(f.h, "POST", "/auth/oauth/register", body, cookie); result.Code != 409 {
		t.Fatal(result.Body.String())
	}
	if f.accounts.registerCalls != 1 {
		t.Fatal("confirmation recreated account")
	}
	f.accounts.account = iamclient.OAuthResult{Authenticated: true, Username: "alice", UID: "uid"}
	f.accounts.user = iamv1.User{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "uid"},
		Spec:       iamv1.UserSpec{Enabled: ptr.To(true), Scopes: []string{string(identity.UserScope)}},
	}
	assertToken(t, f, serve(f.h, "POST", "/auth/oauth/register", body, cookie), "alice")
	f.accounts.user.UID = "recreated"
	if result = serve(f.h, "POST", "/auth/oauth/register", body, cookie); result.Code != 401 {
		t.Fatal("accepted replaced User UID")
	}
}

func TestCallbackBindingAndExpiry(t *testing.T) {
	f := newFixture(t)
	cookie, state := f.login(t)
	for _, body := range []string{
		`{"code":"code","state":"wrong"}`,
		`{"code":"code","state":"` + state + `","state":"` + state + `"}`,
		`{"code":"code","state":"` + state + `","error":"denied"}`,
		`{"code":"code","state":"` + state + `","externalIdentity":{}}`,
	} {
		result := serve(f.h, "POST", "/auth/oauth/community/callback", body, cookie)
		if result.Code != 400 && result.Code != 403 {
			t.Fatalf("%d %s", result.Code, result.Body.String())
		}
	}
	if f.tokenCalls.Load() != 0 {
		t.Fatal("invalid callback reached token endpoint")
	}
	f.now = f.now.Add(contextTTL)
	if result := serve(f.h, "POST", "/auth/oauth/community/callback", `{"code":"code","state":"`+state+`"}`, cookie); result.Code != 410 {
		t.Fatal(result.Body.String())
	}
}

func TestOIDCClaimsValidation(t *testing.T) {
	for _, name := range []string{"issuer", "audience", "nonce", "expired", "azp", "userinfo"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.changeClaims = func(c map[string]any) {
				switch name {
				case "issuer":
					c["iss"] = "https://other.example.com"
				case "audience":
					c["aud"] = "other"
				case "nonce":
					c["nonce"] = "wrong"
				case "expired":
					c["exp"] = f.now.Add(-2 * time.Minute).Unix()
				case "azp":
					c["aud"] = []string{"client", "other"}
					c["azp"] = "other"
				}
			}
			if name == "userinfo" {
				f.userSub = "other-sub"
			}
			result := f.callback(t)
			if result.Code != 401 || f.accounts.authCalls != 0 {
				t.Fatalf("%d %s", result.Code, result.Body.String())
			}
		})
	}
}

func TestProviderDisabled(t *testing.T) {
	f := newFixture(t)
	f.h.config = Config{}
	if result := serve(f.h, "GET", "/auth/oauth/community/login", ""); result.Code != 404 {
		t.Fatal(result.Body.String())
	}
	result := serve(f.h, "GET", "/auth/providers", "")
	if !strings.Contains(result.Body.String(), `"providers":[]`) {
		t.Fatal(result.Body.String())
	}
}

func TestCookieAPIsRequireSameOrigin(t *testing.T) {
	f := newFixture(t)
	cookie, state := f.login(t)
	for _, origin := range []string{"", "https://other.example.com", f.h.config.PublicBaseURL + "/"} {
		router := gin.New()
		router.POST("/auth/oauth/:provider/callback", f.h.Callback)
		req := httptest.NewRequest("POST", "/auth/oauth/community/callback", strings.NewReader(`{"code":"code","state":"`+state+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		req.AddCookie(cookie)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != 403 || f.tokenCalls.Load() != 0 {
			t.Fatal("cross-origin callback reached provider")
		}
	}
}

func TestCallbackOnAnotherReplica(t *testing.T) {
	f := newFixture(t)
	cookie, state := f.login(t)
	other := New(f.h.config, f.h.accounts, f.h.tokens, f.h.now)
	other.provider = newProvider(f.h.config, f.h.now, f.server.Client().Transport)
	response := serve(other, "POST", "/auth/oauth/community/callback", `{"code":"code","state":"`+state+`"}`, cookie)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"registrationRequired":true`) {
		t.Fatal(response.Body.String())
	}
}

func TestOAuthRateLimitIsBounded(t *testing.T) {
	l := rateLimits{entries: make(map[string]rateEntry)}
	now := time.Now()
	for range 10 {
		if !l.allow("login/ip", 10, now) {
			t.Fatal("premature rate limit")
		}
	}
	if l.allow("login/ip", 10, now) {
		t.Fatal("rate limit exceeded")
	}
	for i := range 5000 {
		l.allow(fmt.Sprint(i), 10, now)
	}
	if len(l.entries) != 4096 {
		t.Fatal("limiter memory is not bounded")
	}
	if !l.allow("new", 10, now.Add(time.Minute)) {
		t.Fatal("expired limit entries not recovered")
	}
}

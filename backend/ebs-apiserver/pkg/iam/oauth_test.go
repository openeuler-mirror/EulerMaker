package iam

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emicklei/go-restful/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/authentication/user"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"

	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/iam/credential"
	userstore "ebs-apiserver/pkg/registry/iam/user"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

const gatewayPrincipal = "spiffe://eulermaker/internal/ebs-gateway"

type oauthDocument struct {
	data json.RawMessage
	seq  int64
}

// oauthBackend models realtime reads and atomic create/CAS writes shared by apiserver replicas.
type oauthBackend struct {
	mu               sync.Mutex
	docs             map[string]oauthDocument
	requests         []string
	fail             func(*http.Request, bool) bool
	missingClaimRead func()
}

func (b *oauthBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	var afterRead func()
	defer func() {
		b.mu.Unlock()
		if afterRead != nil {
			afterRead()
		}
	}()
	b.requests = append(b.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if b.fail != nil && b.fail(r, false) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":"unavailable"}`)
		return
	}
	doc, exists := b.docs[r.URL.Path]
	switch r.Method {
	case http.MethodGet:
		if !exists {
			if strings.HasPrefix(r.URL.Path, "/ebs-oauth-identity-claims/") {
				afterRead = b.missingClaimRead
			}
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"found":false}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"_source": doc.data, "_seq_no": doc.seq, "_primary_term": 1, "found": true,
		})
	case http.MethodPut, http.MethodDelete:
		if r.URL.Query().Get("op_type") == "create" && exists {
			w.WriteHeader(409)
			return
		}
		if seq := r.URL.Query().Get("if_seq_no"); seq != "" {
			if !exists || seq != strconv.FormatInt(doc.seq, 10) || r.URL.Query().Get("if_primary_term") != "1" {
				w.WriteHeader(409)
				return
			}
		} else if r.Method == http.MethodDelete || r.URL.Query().Get("op_type") != "create" {
			w.WriteHeader(400)
			return
		}
		if r.Method == http.MethodDelete {
			delete(b.docs, r.URL.Path)
		} else {
			data, _ := io.ReadAll(r.Body)
			doc = oauthDocument{data: data, seq: doc.seq + 1}
			b.docs[r.URL.Path] = doc
		}
		if b.fail != nil && b.fail(r, true) {
			w.WriteHeader(503)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"_seq_no": doc.seq, "_primary_term": 1})
	default:
		w.WriteHeader(405)
	}
}

type oauthFixture struct {
	backend *oauthBackend
	routes  *restful.Container
	client  *es.Client
	users   *esstore.Store
	creds   *credential.Store
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	b := &oauthBackend{docs: map[string]oauthDocument{}}
	srv := httptest.NewServer(b)
	t.Cleanup(srv.Close)
	client := es.NewClientForTesting(srv.URL, srv.Client())
	users := esstore.New(client, "user", "User", userstore.NewStorage().User.(*genericregistry.Store))
	creds := credential.NewStore(client)
	routes := restful.NewContainer()
	server := &genericapiserver.GenericAPIServer{Handler: &genericapiserver.APIServerHandler{GoRestfulContainer: routes}}
	InstallInternalRoutes(server, creds, users, &fakeMachineStorage{}, client)
	return &oauthFixture{b, routes, client, users, creds}
}

func (f *oauthFixture) call(action, principal, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/internal/iam/v1/users/"+action, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if principal != "" {
		req = req.WithContext(request.WithUser(req.Context(), &user.DefaultInfo{Name: principal}))
	}
	rec := httptest.NewRecorder()
	f.routes.ServeHTTP(rec, req)
	return rec
}

func oauthBody(username string) string {
	fields := map[string]interface{}{
		"method": "oauth", "externalIdentity": oauthIdentity{"https://provider.example.com", "123"},
	}
	if username != "" {
		fields["username"] = username
	}
	data, _ := json.Marshal(fields)
	return string(data)
}

func assertOAuthResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body)
	}
	if rec.Code != status || (code != "" && body["code"] != code) {
		t.Fatalf("response %d %s, want %d %s", rec.Code, rec.Body, status, code)
	}
	if code != "" && (len(body) != 2 || body["error"] == nil) {
		t.Fatalf("unexpected error envelope: %v", body)
	}
}

func (f *oauthFixture) userDocument(t *testing.T, name string) es.Document {
	t.Helper()
	hit, err := f.client.Get(context.Background(), "user", name)
	if err != nil {
		t.Fatal(err)
	}
	return hit.Document
}

func TestOAuthRegistrationAndAuthentication(t *testing.T) {
	f := newOAuthFixture(t)
	first := f.call("authenticate", gatewayPrincipal, oauthBody(""))
	assertOAuthResponse(t, first, 200, "")
	if !strings.Contains(first.Body.String(), `"registrationRequired": true`) || len(f.backend.docs) != 0 {
		t.Fatalf("unexpected first authentication: %s", first.Body)
	}
	created := f.call("register", gatewayPrincipal, oauthBody("admin"))
	assertOAuthResponse(t, created, 201, "")
	doc := f.userDocument(t, "admin")
	var account iamv1.User
	_ = json.Unmarshal(doc.Data, &account)
	claim, err := f.client.GetOAuthIdentity(context.Background(), identityKey(oauthIdentity{"https://provider.example.com", "123"}))
	if err != nil {
		t.Fatal(err)
	}
	if account.UID == "" || string(account.UID) != claim.Claim.UserUID || account.Spec.Scopes[0] != "ebs:user" || len(doc.Credential) != 0 {
		t.Fatalf("incorrect OAuth account: %+v, claim: %+v", account, claim)
	}
	if doc.ExternalIdentity == nil || doc.ExternalIdentity.Subject != "123" {
		t.Fatalf("missing private identity: %+v", doc)
	}
	if _, err := time.Parse(time.RFC3339, doc.ExternalIdentity.LinkedAt); err != nil {
		t.Fatal(err)
	}
	retry := strings.TrimSuffix(oauthBody("admin"), "}") + `,"displayName":"changed","email":"new@example.com"}`
	assertOAuthResponse(t, f.call("register", gatewayPrincipal, retry), 200, "")
	if string(f.userDocument(t, "admin").Data) != string(doc.Data) {
		t.Fatal("registration retry changed the profile")
	}
	assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("other")), 409, "IdentityAlreadyLinked")
	assertOAuthResponse(t, f.call("authenticate", gatewayPrincipal, oauthBody("")), 200, "")
	obj, err := f.users.Get(context.Background(), "admin", nil)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(obj)
	if strings.Contains(string(public), "externalIdentity") {
		t.Fatal("private identity exposed in public User")
	}
}

func TestOAuthValidatesBeforeStorage(t *testing.T) {
	for _, body := range []string{
		`{"method":"unknown"}`, `{"method":null}`, `{"method":"oauth","externalIdentity":null}`,
		strings.Replace(oauthBody("alice"), `"alice"`, `"INVALID"`, 1),
		strings.Replace(oauthBody("alice"), `"123"`, `"bad\nsubject"`, 1),
		strings.Replace(oauthBody("alice"), `"123"`, `"\ud800"`, 1),
		strings.Replace(oauthBody("alice"), `"123"`, `"\udfff"`, 1),
		strings.Replace(oauthBody("alice"), `"123"`, `123`, 1),
		strings.Replace(oauthBody("alice"), `"123"`, `"`+strings.Repeat("x", 1025)+`"`, 1),
		strings.TrimSuffix(oauthBody("alice"), "}") + `,"password":"password"}`,
		strings.TrimSuffix(oauthBody("alice"), "}") + `,"scopes":["ebs:admin"]}`,
		strings.TrimSuffix(oauthBody("alice"), "}") + `,"email":"not an email"}`,
		strings.TrimSuffix(oauthBody("alice"), "}") + `,"displayName":null}`,
		strings.Replace(oauthBody("alice"), `"subject":"123"`, `"subject":"123","token":"secret"`, 1),
	} {
		f := newOAuthFixture(t)
		assertOAuthResponse(t, f.call("register", gatewayPrincipal, body), 400, "InvalidRequest")
		if len(f.backend.requests) != 0 {
			t.Fatalf("invalid input reached storage: %v", f.backend.requests)
		}
	}
	for _, principal := range []string{"", "system:anonymous", "spiffe://eulermaker/internal/scheduler"} {
		f := newOAuthFixture(t)
		status, code := 403, "Forbidden"
		if principal == "" || principal == "system:anonymous" {
			status, code = 401, "Unauthorized"
		}
		for _, action := range []string{"register", "authenticate"} {
			name := ""
			if action == "register" {
				name = "alice"
			}
			assertOAuthResponse(t, f.call(action, principal, oauthBody(name)), status, code)
		}
		if len(f.backend.requests) != 0 {
			t.Fatal("unauthorized caller reached storage")
		}
	}
}

func TestOAuthConcurrentRegistration(t *testing.T) {
	f := newOAuthFixture(t)
	var initialReads sync.WaitGroup
	initialReads.Add(2)
	f.backend.missingClaimRead = func() {
		initialReads.Done()
		initialReads.Wait()
	}
	// Separate service instances share only ES, not a process-local registration lock.
	services := []*oauthService{{client: f.client, users: f.users}, {client: f.client, users: f.users}}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, service := range services {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := service.register(context.Background(), userRequest{
				Username: []string{"alice", "bob"}[i], Identity: oauthIdentity{"https://provider.example.com", "123"},
			})
			if err != nil && err.Error() != "IdentityAlreadyLinked" && err.Error() != "RegistrationInProgress" {
				t.Errorf("register: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(f.backend.docs) != 2 {
		t.Fatalf("want one claim and one User, got %d documents", len(f.backend.docs))
	}
	userCreates := 0
	for _, operation := range f.backend.requests {
		if strings.HasPrefix(operation, "PUT /ebs-users/") {
			userCreates++
		}
	}
	if userCreates != 1 {
		t.Fatalf("created User %d times", userCreates)
	}
}

func TestOAuthUnknownWriteConfirmation(t *testing.T) {
	for _, test := range []struct {
		name, path string
		persisted  bool
		status     int
		code       string
		wantDocs   int
	}{
		{"claim not written", "/ebs-oauth-identity-claims/", false, 503, "IAMUnavailable", 0},
		{"claim response lost", "/ebs-oauth-identity-claims/", true, 409, "RegistrationInProgress", 1},
		{"user not written", "/ebs-users/", false, 409, "RegistrationInProgress", 1},
		{"user response lost", "/ebs-users/", true, 200, "", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newOAuthFixture(t)
			writes := 0
			f.backend.fail = func(r *http.Request, persisted bool) bool {
				if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, test.path) && persisted == test.persisted {
					writes++
					return true
				}
				return false
			}
			assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), test.status, test.code)
			if writes != 1 || len(f.backend.docs) != test.wantDocs {
				t.Fatalf("writes=%d documents=%d", writes, len(f.backend.docs))
			}
			if test.wantDocs > 0 {
				f.backend.fail = nil
				assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), test.status, test.code)
			}
		})
	}
}

func TestOAuthUsernameConflictReleasesOwnedClaim(t *testing.T) {
	for _, releaseFails := range []bool{false, true} {
		f := newOAuthFixture(t)
		assertOAuthResponse(t, f.call("register", gatewayPrincipal, `{"username":"alice","password":"long password"}`), 201, "")
		if releaseFails {
			f.backend.fail = func(r *http.Request, persisted bool) bool { return r.Method == http.MethodDelete && !persisted }
		}
		if releaseFails {
			assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), 503, "IAMUnavailable")
			if len(f.backend.docs) != 2 {
				t.Fatal("failed release lost the claim")
			}
			assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("bob")), 500, "IdentityStateInvalid")
		} else {
			assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), 409, "UsernameAlreadyExists")
			if len(f.backend.docs) != 1 {
				t.Fatal("confirmed username rejection retained claim")
			}
			assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("bob")), 201, "")
		}
	}
}

func TestOAuthPrivateIdentitySurvivesUpdates(t *testing.T) {
	f := newOAuthFixture(t)
	assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), 201, "")
	identity := *f.userDocument(t, "alice").ExternalIdentity
	obj, err := f.users.Get(context.Background(), "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	obj.(*iamv1.User).Spec.DisplayName = "Alice"
	ctx := request.WithNamespace(context.Background(), "")
	if _, _, err := f.users.Update(ctx, "alice", rest.DefaultUpdatedObjectInfo(obj), nil, nil, false, &metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.creds.SetPassword(context.Background(), "alice", "long password"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if ok, err := f.creds.Authenticate(context.Background(), "alice", "incorrect"); ok || err != nil {
			t.Fatalf("password failure = %t, %v", ok, err)
		}
	}
	before := f.userDocument(t, "alice")
	assertOAuthResponse(t, f.call("authenticate", gatewayPrincipal, oauthBody("")), 200, "")
	after := f.userDocument(t, "alice")
	if *after.ExternalIdentity != identity || string(before.Credential) != string(after.Credential) {
		t.Fatal("identity or password lock modified")
	}
	if ok, err := f.creds.Authenticate(context.Background(), "alice", "long password"); ok || err != nil {
		t.Fatalf("password lock = %t, %v", ok, err)
	}
}

func TestOAuthAccountState(t *testing.T) {
	for _, state := range []string{"disabled", "invalid scopes", "deleted", "replaced"} {
		t.Run(state, func(t *testing.T) {
			f := newOAuthFixture(t)
			assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), 201, "")
			if err := f.creds.SetPassword(context.Background(), "alice", "long password"); err != nil {
				t.Fatal(err)
			}
			doc := f.userDocument(t, "alice")
			var account iamv1.User
			_ = json.Unmarshal(doc.Data, &account)
			status, code := 401, "AuthenticationFailed"
			switch state {
			case "disabled":
				*account.Spec.Enabled = false
			case "invalid scopes":
				account.Spec.Scopes = []string{"ebs:system"}
			case "deleted":
				status, code = 409, "RegistrationInProgress"
			case "replaced":
				account.UID = "replacement"
				status, code = 500, "IdentityStateInvalid"
			}
			doc.Data, _ = json.Marshal(account)
			data, _ := json.Marshal(doc)
			f.backend.docs["/ebs-users/_doc/alice"] = oauthDocument{data: data, seq: 2}
			if state == "deleted" {
				delete(f.backend.docs, "/ebs-users/_doc/alice")
			}
			assertOAuthResponse(t, f.call("authenticate", gatewayPrincipal, oauthBody("")), status, code)
			if status == 401 {
				status, code = 403, "AccountUnavailable"
			}
			assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), status, code)
			if state == "disabled" || state == "invalid scopes" {
				if ok, err := f.creds.Authenticate(context.Background(), "alice", "long password"); ok || err != nil {
					t.Fatalf("unavailable account password authentication = %t, %v", ok, err)
				}
			}
		})
	}
}

func TestOAuthIdentityKey(t *testing.T) {
	if got := identityKey(oauthIdentity{"a", "bc"}); got != "3fafa1cf2f19a7c1129beb20cf0983f73a489a221fc0dd2f16d1be292d089205" {
		t.Fatalf("identity key: %s", got)
	}
	if identityKey(oauthIdentity{"a", "bc"}) == identityKey(oauthIdentity{"ab", "c"}) {
		t.Fatal("ambiguous identity encoding")
	}
}

func TestOAuthUnicodeIdentity(t *testing.T) {
	for _, subject := range []string{`"\ud83d\ude00"`, `"😀"`, `"�"`, `"\\ud800"`} {
		body := strings.Replace(oauthBody("alice"), `"123"`, subject, 1)
		f := newOAuthFixture(t)
		assertOAuthResponse(t, f.call("register", gatewayPrincipal, body), 201, "")
		assertOAuthResponse(t, f.call("register", gatewayPrincipal, body), 200, "")
	}
}

func TestOAuthMissingIndexIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected write after missing index: %s", r.Method)
		}
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"type":"index_not_found_exception"}}`)
	}))
	defer srv.Close()
	service := oauthService{client: es.NewClientForTesting(srv.URL, srv.Client())}
	_, _, err := service.register(context.Background(), userRequest{Username: "alice", Identity: oauthIdentity{"issuer", "subject"}})
	if err == nil || err.Error() != "IAMUnavailable" {
		t.Fatalf("registration with missing identity index = %v", err)
	}
}

func TestOAuthClaimReleaseDoesNotDeleteChangedVersion(t *testing.T) {
	f := newOAuthFixture(t)
	assertOAuthResponse(t, f.call("register", gatewayPrincipal, `{"username":"alice","password":"long password"}`), 201, "")
	f.backend.fail = func(r *http.Request, persisted bool) bool {
		if r.Method == http.MethodDelete && !persisted {
			doc := f.backend.docs[r.URL.Path]
			doc.seq++
			f.backend.docs[r.URL.Path] = doc
		}
		return false
	}
	assertOAuthResponse(t, f.call("register", gatewayPrincipal, oauthBody("alice")), 503, "IAMUnavailable")
	if len(f.backend.docs) != 2 {
		t.Fatal("CAS conflict removed the identity claim")
	}
}

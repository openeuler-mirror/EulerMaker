package iam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/emicklei/go-restful/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"

	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/iam/credential"
	"ebs-apiserver/pkg/storage/es"
)

type fakeUserStorage struct {
	user       *iamv1.User
	deleted    bool
	credential json.RawMessage
}

type fakeMachineStorage struct {
	account    *iamv1.MachineAccount
	credential json.RawMessage
}

func (f *fakeMachineStorage) CreateWithCredential(_ context.Context, obj runtime.Object, data json.RawMessage, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	f.account = obj.(*iamv1.MachineAccount).DeepCopy()
	f.credential = append(json.RawMessage(nil), data...)
	return f.account.DeepCopy(), nil
}

func (f *fakeUserStorage) CreateWithCredential(ctx context.Context, obj runtime.Object, data json.RawMessage, validate rest.ValidateObjectFunc, opts *metav1.CreateOptions) (runtime.Object, error) {
	f.credential = append(json.RawMessage(nil), data...)
	return f.Create(ctx, obj, validate, opts)
}

func (f *fakeUserStorage) CreateWithExternalIdentity(ctx context.Context, obj runtime.Object, _ *es.ExternalIdentity) (runtime.Object, error) {
	return f.Create(ctx, obj, nil, nil)
}

func (f *fakeUserStorage) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	f.user = obj.(*iamv1.User).DeepCopy()
	return f.user.DeepCopy(), nil
}

func (f *fakeUserStorage) Get(_ context.Context, _ string, _ *metav1.GetOptions) (runtime.Object, error) {
	return f.user.DeepCopy(), nil
}

func (f *fakeUserStorage) Update(_ context.Context, _ string, info rest.UpdatedObjectInfo, _ rest.ValidateObjectFunc, _ rest.ValidateObjectUpdateFunc, _ bool, _ *metav1.UpdateOptions) (runtime.Object, bool, error) {
	obj, err := info.UpdatedObject(context.Background(), f.user.DeepCopy())
	if err != nil {
		return nil, false, err
	}
	f.user = obj.(*iamv1.User).DeepCopy()
	return f.user.DeepCopy(), false, nil
}

func (f *fakeUserStorage) Delete(_ context.Context, _ string, _ rest.ValidateObjectFunc, _ *metav1.DeleteOptions) (runtime.Object, bool, error) {
	f.deleted = true
	return f.user.DeepCopy(), true, nil
}

func TestRegisterCreatesEnabledUserAndCredential(t *testing.T) {
	users := &fakeUserStorage{}
	h := &Handler{users: users}
	req := httptest.NewRequest(http.MethodPost, "/internal/iam/v1/users/register", strings.NewReader(`{"username":"alice","password":"correct password","displayName":"Alice","email":"alice@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	response := restful.NewResponse(rec)
	response.SetRequestAccepts(restful.MIME_JSON)
	h.register(restful.NewRequest(req), response)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if users.user.Spec.Enabled == nil || !*users.user.Spec.Enabled || len(users.user.Spec.Scopes) != 1 || users.user.Spec.Scopes[0] != "ebs:user" || users.user.Annotations != nil {
		t.Fatalf("user was not finalized: %#v", users.user)
	}
	if len(users.credential) == 0 {
		t.Fatal("credential was not created")
	}
}

func TestRegisterAdminReceivesAdminScope(t *testing.T) {
	users := &fakeUserStorage{}
	h := &Handler{users: users}
	req := httptest.NewRequest(http.MethodPost, "/internal/iam/v1/users/register", strings.NewReader(`{"username":"admin","password":"correct password"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	response := restful.NewResponse(rec)
	response.SetRequestAccepts(restful.MIME_JSON)
	h.register(restful.NewRequest(req), response)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if users.user == nil || len(users.user.Spec.Scopes) != 1 || users.user.Spec.Scopes[0] != "ebs:admin" {
		t.Fatalf("admin registration has wrong scope: %#v", users.user)
	}
}

func TestValidRegistration(t *testing.T) {
	if !validRegistration("alice", "correct password", "alice@example.com") {
		t.Fatal("valid registration rejected")
	}
	for _, input := range [][3]string{
		{"Alice", "correct password", ""},
		{"alice", "short", ""},
		{"alice", "correct password", "invalid"},
	} {
		if validRegistration(input[0], input[1], input[2]) {
			t.Fatalf("invalid registration accepted: %#v", input)
		}
	}
}

func TestRegisterMachineAccount(t *testing.T) {
	for _, tc := range []struct {
		name, typeField string
		wantType        iamv1.MachineAccountType
		status          int
	}{
		{"default", "", iamv1.MachineAccountRunner, http.StatusCreated},
		{"runner", `,"type":"runner"`, iamv1.MachineAccountRunner, http.StatusCreated},
		{"service", `,"type":"service"`, iamv1.MachineAccountService, http.StatusCreated},
		{"empty", `,"type":""`, "", http.StatusBadRequest},
		{"null", `,"type":null`, "", http.StatusBadRequest},
		{"unknown", `,"type":"admin"`, "", http.StatusBadRequest},
		{"wrong JSON type", `,"type":1`, "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			machines := &fakeMachineStorage{}
			h := &Handler{machines: machines}
			secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
			body := `{"name":"runner-site-a","clientSecret":"` + secret + `","tokenTTLSeconds":86400` + tc.typeField + `}`
			req := httptest.NewRequest(http.MethodPost, "/internal/iam/v1/machineaccounts/register", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			response := restful.NewResponse(rec)
			response.SetRequestAccepts(restful.MIME_JSON)
			h.registerMachine(restful.NewRequest(req), response)
			if rec.Code != tc.status {
				t.Fatalf("expected %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
			}
			if tc.status != http.StatusCreated {
				if machines.account != nil || len(machines.credential) != 0 {
					t.Fatal("invalid type was persisted")
				}
				return
			}
			if machines.account == nil || machines.account.Name != "runner-site-a" || machines.account.Spec.TokenTTLSeconds != 86400 || machines.account.Spec.Type != tc.wantType {
				t.Fatalf("unexpected account: %#v", machines.account)
			}
			if len(machines.credential) == 0 {
				t.Fatal("credential was not stored")
			}
		})
	}
}

func TestRegisterMachineGrants(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		status       int
	}{
		{"service grants", `"type":"service","projects":["demo","other"],"permissions":["projects:update","builds:create","builds:abort"]`, http.StatusCreated},
		{"abort only", `"type":"service","projects":["demo"],"permissions":["builds:abort"]`, http.StatusCreated},
		{"empty grants", `"type":"service","projects":[],"permissions":[]`, http.StatusCreated},
		{"all projects", `"type":"service","projects":["*"],"permissions":["projects:update"]`, http.StatusCreated},
		{"mixed wildcard scope", `"type":"service","projects":["*","demo"],"permissions":["projects:update"]`, http.StatusBadRequest},
		{"invalid project", `"type":"service","projects":["demo-*"]`, http.StatusBadRequest},
		{"duplicate projects", `"type":"service","projects":["demo","demo"]`, http.StatusBadRequest},
		{"unknown permission", `"type":"service","permissions":["projects:delete"]`, http.StatusBadRequest},
		{"duplicate permissions", `"type":"service","permissions":["builds:create","builds:create"]`, http.StatusBadRequest},
		{"default runner grants", `"projects":["demo"]`, http.StatusBadRequest},
		{"runner grants", `"type":"runner","permissions":["builds:abort"]`, http.StatusBadRequest},
		{"projects scalar", `"type":"service","projects":"demo"`, http.StatusBadRequest},
		{"permissions object", `"type":"service","permissions":{}`, http.StatusBadRequest},
		{"null project entry", `"type":"service","projects":[null]`, http.StatusBadRequest},
		{"null permission entry", `"type":"service","permissions":[null]`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			machines := &fakeMachineStorage{}
			h := &Handler{machines: machines}
			secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
			body := `{"name":"automation","clientSecret":"` + secret + `",` + test.fields + `}`
			req := httptest.NewRequest(http.MethodPost, "/internal/iam/v1/machineaccounts/register", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			response := restful.NewResponse(rec)
			response.SetRequestAccepts(restful.MIME_JSON)
			h.registerMachine(restful.NewRequest(req), response)
			if rec.Code != test.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if test.status != http.StatusCreated {
				if machines.account != nil || len(machines.credential) != 0 {
					t.Fatal("invalid grants reached storage")
				}
				return
			}
			want := iamv1.MachineAccountSpec{TokenTTLSeconds: 3600}
			if err := json.Unmarshal([]byte("{"+test.fields+"}"), &want); err != nil {
				t.Fatal(err)
			}
			if machines.account == nil || !reflect.DeepEqual(machines.account.Spec, want) || len(machines.credential) == 0 {
				t.Fatalf("account or credential missing: %+v", machines.account)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatal("registration response leaked secret")
			}
		})
	}
}

func TestInternalAuthenticationRoutes(t *testing.T) {
	password := "correct password"
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	userCredential, err := credential.NewPasswordCredential(password)
	if err != nil {
		t.Fatal(err)
	}
	machineCredential, err := credential.NewMachineCredential(secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		path     string
		body     string
		esPath   string
		document es.IAMDocument
		response string
	}{
		{
			name: "user", path: "/internal/iam/v1/users/authenticate",
			body:   `{"username":"alice","password":"` + password + `"}`,
			esPath: "/ebs-users/_doc/alice",
			document: es.IAMDocument{
				Document:   es.Document{Data: json.RawMessage(`{"spec":{"enabled":true,"scopes":["ebs:user"]}}`)},
				Credential: userCredential,
			},
			response: `{"authenticated":true,"username":"alice"}`,
		},
		{
			name: "machine", path: "/internal/iam/v1/machineaccounts/authenticate",
			body:   `{"name":"worker","clientSecret":"` + secret + `"}`,
			esPath: "/ebs-machineaccounts/_doc/worker",
			document: es.IAMDocument{
				Document:   es.Document{Data: json.RawMessage(`{"metadata":{"name":"worker"},"spec":{"tokenTTLSeconds":3600}}`)},
				Credential: machineCredential,
			},
			response: `{"authenticated":true,"name":"worker","type":"runner","tokenTTLSeconds":3600}`,
		},
		{
			name: "service", path: "/internal/iam/v1/machineaccounts/authenticate",
			body:   `{"name":"worker","clientSecret":"` + secret + `"}`,
			esPath: "/ebs-machineaccounts/_doc/worker",
			document: es.IAMDocument{
				Document:   es.Document{Data: json.RawMessage(`{"metadata":{"name":"worker"},"spec":{"type":"service","tokenTTLSeconds":3600,"projects":["demo"],"permissions":["builds:abort"]}}`)},
				Credential: machineCredential,
			},
			response: `{"authenticated":true,"name":"worker","type":"service","tokenTTLSeconds":3600}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != tc.esPath {
					t.Errorf("unexpected ES request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"_seq_no": 0, "_primary_term": 1, "_source": tc.document,
				})
			}))
			defer backend.Close()
			container := restful.NewContainer()
			server := &genericapiserver.GenericAPIServer{
				Handler: &genericapiserver.APIServerHandler{GoRestfulContainer: container},
			}
			credentials := credential.NewStore(es.NewClientForTesting(backend.URL, backend.Client()))
			InstallInternalRoutes(server, credentials, &fakeUserStorage{}, &fakeMachineStorage{}, es.NewClientForTesting(backend.URL, backend.Client()), false)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			container.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("authentication status=%d body=%s", rec.Code, rec.Body.String())
			}
			var got, want map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.response), &want); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("response=%v want=%v", got, want)
			}
			for key, value := range want {
				if got[key] != value {
					t.Errorf("response[%s]=%v want=%v", key, got[key], value)
				}
			}
		})
	}
}

package iam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emicklei/go-restful/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/registry/rest"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
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
	if users.user.Spec.Enabled == nil || !*users.user.Spec.Enabled || users.user.Spec.Admin || users.user.Annotations != nil {
		t.Fatalf("user was not finalized: %#v", users.user)
	}
	if len(users.credential) == 0 {
		t.Fatal("credential was not created")
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
	machines := &fakeMachineStorage{}
	h := &Handler{machines: machines}
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	body := `{"name":"runner-site-a","clientSecret":"` + secret + `","tokenTTLSeconds":86400}`
	req := httptest.NewRequest(http.MethodPost, "/internal/iam/v1/machineaccounts/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	response := restful.NewResponse(rec)
	response.SetRequestAccepts(restful.MIME_JSON)
	h.registerMachine(restful.NewRequest(req), response)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if machines.account == nil || machines.account.Name != "runner-site-a" || machines.account.Spec.TokenTTLSeconds != 86400 {
		t.Fatalf("unexpected account: %#v", machines.account)
	}
	if len(machines.credential) == 0 {
		t.Fatal("credential was not stored")
	}
}

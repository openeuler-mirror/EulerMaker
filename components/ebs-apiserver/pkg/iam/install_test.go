package iam

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emicklei/go-restful/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/registry/rest"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
	"ebs-apiserver/pkg/iam/credential"
	"ebs-apiserver/pkg/storage/es"
)

type fakeUserStorage struct {
	user    *iamv1.User
	deleted bool
}

func (f *fakeUserStorage) Create(_ context.Context, obj runtime.Object, _ rest.ValidateObjectFunc, _ *metav1.CreateOptions) (runtime.Object, error) {
	f.user = obj.(*iamv1.User).DeepCopy()
	f.user.UID = types.UID("registration-test")
	f.user.ResourceVersion = "v1:0:1"
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
	var credentialDocument es.Document
	httpClient := &http.Client{Transport: iamRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/ebs-users/"):
			return iamHTTPResponse(http.StatusOK, `{"_id":"alice","_seq_no":0,"_primary_term":1,"_source":{"apiVersion":"iam.ebs/v1","kind":"User","documentID":"alice","metadata":{"name":"alice"},"data":{"metadata":{"name":"alice"},"spec":{"enabled":false}}}}`), nil
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/ebs-user-credentials/"):
			return iamHTTPResponse(http.StatusNotFound, `{"error":"missing"}`), nil
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/ebs-user-credentials/"):
			if err := json.NewDecoder(req.Body).Decode(&credentialDocument); err != nil {
				t.Fatalf("decode credential: %v", err)
			}
			return iamHTTPResponse(http.StatusCreated, `{"_seq_no":0,"_primary_term":1}`), nil
		default:
			t.Fatalf("unexpected ES request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}
	users := &fakeUserStorage{}
	h := &Handler{credentials: credential.NewStore(es.NewClientForTesting("http://elasticsearch", httpClient)), users: users}
	req := httptest.NewRequest(http.MethodPost, "/internal/iam/v1/register", strings.NewReader(`{"username":"alice","password":"correct password","displayName":"Alice","email":"alice@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	response := restful.NewResponse(rec)
	response.SetRequestAccepts(restful.MIME_JSON)
	h.register(restful.NewRequest(req), response)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	if users.user.Spec.Enabled == nil || !*users.user.Spec.Enabled || users.user.Annotations != nil {
		t.Fatalf("user was not finalized: %#v", users.user)
	}
	if credentialDocument.DocumentID != "alice" || len(credentialDocument.Data) == 0 {
		t.Fatalf("credential was not created: %#v", credentialDocument)
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

type iamRoundTripFunc func(*http.Request) (*http.Response, error)

func (f iamRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func iamHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

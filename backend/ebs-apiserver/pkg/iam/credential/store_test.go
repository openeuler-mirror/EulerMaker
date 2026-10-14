package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ebs-apiserver/pkg/storage/es"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func httpResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format: %s", hash)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("verify correct password: ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("incorrect password", hash)
	if err != nil {
		t.Fatalf("verify incorrect password: %v", err)
	}
	if ok {
		t.Fatal("incorrect password authenticated")
	}
}

func TestVerifyPasswordRejectsInvalidEncoding(t *testing.T) {
	for _, encoded := range []string{"", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=0,t=2,p=1$AA$AA"} {
		if _, err := VerifyPassword("password", encoded); err == nil {
			t.Errorf("expected error for %q", encoded)
		}
	}
}

func TestPasswordLengthPolicy(t *testing.T) {
	store := &Store{}
	if err := store.SetPassword(context.Background(), "alice", "short"); err != ErrInvalidPassword {
		t.Fatalf("got %v, want ErrInvalidPassword", err)
	}
}

func TestAuthenticateRejectsDisabledUser(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	credentialData, _ := json.Marshal(Credential{PasswordHash: hash, PasswordUpdatedAt: time.Now().UTC()})
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/ebs-users/") {
			doc := es.Document{APIVersion: "iam.ebs/v1", Kind: "User", DocumentID: "alice", Metadata: es.Metadata{Name: "alice"}, Data: json.RawMessage(`{"metadata":{"name":"alice"},"spec":{"enabled":false}}`), Credential: credentialData}
			body, _ := json.Marshal(map[string]interface{}{"_id": "alice", "_seq_no": 0, "_primary_term": 1, "_source": doc})
			return httpResponse(http.StatusOK, string(body)), nil
		} else {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}
	store := NewStore(es.NewClientForTesting("http://elasticsearch", httpClient))
	ok, err := store.Authenticate(context.Background(), "alice", "correct horse battery staple")
	if err != nil {
		t.Fatalf("authenticate disabled user: %v", err)
	}
	if ok {
		t.Fatal("disabled registration user authenticated")
	}
}

func TestCredentialLifecycleAndLockout(t *testing.T) {
	credentialData, _ := NewPasswordCredential("initial password value")
	userDoc := es.Document{APIVersion: "iam.ebs/v1", Kind: "User", DocumentID: "alice", Metadata: es.Metadata{Name: "alice"}, Data: json.RawMessage(`{"metadata":{"name":"alice"},"spec":{"enabled":true}}`), Credential: credentialData}
	seq := int64(0)
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/ebs-users/"):
			body, _ := json.Marshal(map[string]interface{}{"_id": "alice", "_seq_no": seq, "_primary_term": int64(1), "_source": userDoc})
			return httpResponse(http.StatusOK, string(body)), nil
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/ebs-users/"):
			var doc es.Document
			if err := json.NewDecoder(req.Body).Decode(&doc); err != nil {
				t.Fatalf("decode credential document: %v", err)
			}
			userDoc = doc
			seq++
			body, _ := json.Marshal(map[string]int64{"_seq_no": seq, "_primary_term": 1})
			return httpResponse(http.StatusOK, string(body)), nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}
	store := NewStore(es.NewClientForTesting("http://elasticsearch", httpClient))
	ctx := context.Background()
	if err := store.SetPassword(ctx, "alice", "correct horse battery staple"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	ok, err := store.Authenticate(ctx, "alice", "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("authenticate: ok=%v err=%v", ok, err)
	}
	for i := 0; i < maxFailures; i++ {
		ok, err = store.Authenticate(ctx, "alice", "incorrect password")
		if err != nil || ok {
			t.Fatalf("failed attempt %d: ok=%v err=%v", i+1, ok, err)
		}
	}
	ok, err = store.Authenticate(ctx, "alice", "correct horse battery staple")
	if err != nil {
		t.Fatalf("authenticate locked account: %v", err)
	}
	if ok {
		t.Fatal("locked account authenticated")
	}
}

func TestAuthenticateMachine(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	credentialData, err := NewMachineCredential(secret)
	if err != nil {
		t.Fatalf("new machine credential: %v", err)
	}
	doc := es.Document{APIVersion: "iam.ebs/v1", Kind: "MachineAccount", DocumentID: "runner-site-a", Metadata: es.Metadata{Name: "runner-site-a"}, Data: json.RawMessage(`{"metadata":{"name":"runner-site-a"},"spec":{"tokenTTLSeconds":86400}}`), Credential: credentialData}
	seq := int64(0)
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.Method {
		case http.MethodGet:
			body, _ := json.Marshal(map[string]interface{}{"_id": "runner-site-a", "_seq_no": seq, "_primary_term": int64(1), "_source": doc})
			return httpResponse(http.StatusOK, string(body)), nil
		case http.MethodPut:
			if err := json.NewDecoder(req.Body).Decode(&doc); err != nil {
				t.Fatalf("decode update: %v", err)
			}
			seq++
			return httpResponse(http.StatusOK, `{"_seq_no":1,"_primary_term":1}`), nil
		default:
			t.Fatalf("unexpected request: %s", req.Method)
			return nil, nil
		}
	})}
	store := NewStore(es.NewClientForTesting("http://elasticsearch", httpClient))
	ttl, ok, err := store.AuthenticateMachine(context.Background(), "runner-site-a", secret)
	if err != nil || !ok || ttl != 86400 {
		t.Fatalf("authenticate machine: ttl=%d ok=%v err=%v", ttl, ok, err)
	}
}

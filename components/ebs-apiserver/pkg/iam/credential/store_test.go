package credential

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

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

func TestCredentialLifecycleAndLockout(t *testing.T) {
	var credentialDoc *es.Document
	seq := int64(0)
	httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/ebs-users/"):
			return httpResponse(http.StatusOK, `{"_id":"alice","_seq_no":0,"_primary_term":1,"_source":{"apiVersion":"iam.ebs/v1","kind":"User","documentID":"alice","metadata":{"name":"alice"},"data":{"metadata":{"name":"alice"}}}}`), nil
		case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/ebs-user-credentials/"):
			if credentialDoc == nil {
				return httpResponse(http.StatusNotFound, `{"error":"missing"}`), nil
			}
			body, _ := json.Marshal(map[string]interface{}{"_id": "alice", "_seq_no": seq, "_primary_term": int64(1), "_source": credentialDoc})
			return httpResponse(http.StatusOK, string(body)), nil
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/ebs-user-credentials/"):
			var doc es.Document
			if err := json.NewDecoder(req.Body).Decode(&doc); err != nil {
				t.Fatalf("decode credential document: %v", err)
			}
			credentialDoc = &doc
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

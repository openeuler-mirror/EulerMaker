package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	iamv1 "ebs-api/iam/v1"
	"ebs-apiserver/pkg/storage/es"
)

type personalBackend struct {
	mu                                                sync.Mutex
	doc                                               es.IAMDocument
	seq                                               int64
	reads, writes, conflicts, readStatus, writeStatus int
	onConflict                                        func()
	commitBeforeFailure                               bool
	readErrorBody                                     string
}

func newPersonalStore(t *testing.T) (*Store, *personalBackend) {
	t.Helper()
	b := &personalBackend{doc: es.IAMDocument{
		Document: es.Document{
			Kind: "User", DocumentID: "alice", Metadata: es.Metadata{Name: "alice"},
			Data: json.RawMessage(`{"metadata":{"name":"alice","uid":"uid-a"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`),
		},
		ExternalIdentity: &es.ExternalIdentity{Issuer: "https://example.com", Subject: "123"},
	}}
	store := NewStore(es.NewClientForTesting("http://es", &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		if req.URL.Path != "/ebs-users/_doc/alice" {
			return httpResponse(404, `{"found":false}`), nil
		}
		switch req.Method {
		case http.MethodGet:
			b.reads++
			if b.readStatus != 0 {
				body := b.readErrorBody
				if body == "" {
					body = `{"found":false}`
				}
				return httpResponse(b.readStatus, body), nil
			}
			data, _ := json.Marshal(map[string]interface{}{"_source": b.doc, "_seq_no": b.seq, "_primary_term": 1})
			return httpResponse(200, string(data)), nil
		case http.MethodPut:
			b.writes++
			if b.conflicts > 0 {
				b.conflicts--
				if b.onConflict != nil {
					b.onConflict()
				}
				b.seq++
				return httpResponse(409, `{}`), nil
			}
			if req.URL.Query().Get("if_seq_no") != strconv.FormatInt(b.seq, 10) || req.URL.Query().Get("if_primary_term") != "1" {
				return httpResponse(409, `{}`), nil
			}
			if b.writeStatus == 0 || b.commitBeforeFailure {
				if err := json.NewDecoder(req.Body).Decode(&b.doc); err != nil {
					t.Error(err)
				}
				b.seq++
			}
			if b.writeStatus != 0 {
				return httpResponse(b.writeStatus, `{}`), nil
			}
			return httpResponse(200, `{"_seq_no":`+strconv.FormatInt(b.seq, 10)+`,"_primary_term":1}`), nil
		default:
			t.Errorf("unexpected method %s", req.Method)
			return httpResponse(500, `{}`), nil
		}
	})}))
	store.now = func() time.Time { return time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC) }
	return store, b
}

func personalCreate(t *testing.T, s *Store) *CreatedPersonalCredential {
	t.Helper()
	result, err := s.CreatePersonal(context.Background(), "alice", " 开发机 ", nil)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPersonalCredentialLifecycle(t *testing.T) {
	s, b := newPersonalStore(t)
	ctx := context.Background()
	created := personalCreate(t, s)
	if created.Name != "开发机" || created.ExpiresAt.Sub(created.CreatedAt) != 90*24*time.Hour {
		t.Fatal("incorrect defaults")
	}
	username, id, err := personalTokenParts(created.Token)
	if err != nil || username != "alice" || id != created.ID {
		t.Fatal("invalid generated token")
	}
	digest := sha256.Sum256([]byte(created.Token))
	if strings.Contains(string(b.doc.Credential), created.Token) || !strings.Contains(string(b.doc.Credential), hex.EncodeToString(digest[:])) {
		t.Fatal("credential must contain only the digest")
	}
	result, err := s.AuthenticatePersonal(ctx, created.Token)
	if err != nil || result.Username != "alice" || result.UID != "uid-a" || result.Scopes[0] != "ebs:user" || result.CredentialID != id {
		t.Fatalf("authentication: %+v %v", result, err)
	}
	if ok, err := s.Authenticate(ctx, "alice", "some password"); err != nil || ok {
		t.Fatalf("PAT-only password authentication: %v %v", ok, err)
	}
	for range 4 {
		personalCreate(t, s)
	}
	if result, err := s.CreatePersonal(ctx, "alice", "sixth", nil); result != nil || !errors.Is(err, ErrPersonalLimit) {
		t.Fatalf("limit: %v", err)
	}
	writes := b.writes
	items, err := s.ListPersonal(ctx, "alice")
	if err != nil || len(items) != 5 || b.writes != writes {
		t.Fatalf("read-only list: %v", err)
	}
	data, _ := json.Marshal(items)
	if strings.Contains(string(data), "secretHash") || strings.Contains(string(data), "token") {
		t.Fatal("list exposes secret")
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].ID >= items[i].ID {
			t.Fatal("unstable list ordering")
		}
	}
	for range 2 {
		if err := s.DeletePersonal(ctx, "alice", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AuthenticatePersonal(ctx, created.Token); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatalf("revoked: %v", err)
	}
	personalCreate(t, s)
	s.now = func() time.Time { return created.ExpiresAt }
	items, err = s.ListPersonal(ctx, "alice")
	if err != nil || len(items) != 0 {
		t.Fatalf("expired list: %v", err)
	}
	if _, err := s.AuthenticatePersonal(ctx, created.Token); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatal(err)
	}
	personalCreate(t, s)
	var stored Credential
	if err := json.Unmarshal(b.doc.Credential, &stored); err != nil || len(stored.PersonalCredentials) != 1 {
		t.Fatal("expired entries not pruned")
	}
}

func TestPersonalCredentialValidation(t *testing.T) {
	s, b := newPersonalStore(t)
	ctx := context.Background()
	for _, name := range []string{"", " ", strings.Repeat("字", 65), "a\nb", string([]byte{0xff})} {
		if _, err := s.CreatePersonal(ctx, "alice", name, nil); !errors.Is(err, ErrPersonalInput) {
			t.Errorf("accepted invalid name")
		}
	}
	for _, days := range []int{-1, 0, 91} {
		if _, err := s.CreatePersonal(ctx, "alice", "name", &days); !errors.Is(err, ErrPersonalInput) {
			t.Errorf("accepted %d days", days)
		}
	}
	if b.reads != 0 || b.writes != 0 {
		t.Fatal("invalid input accessed storage")
	}
	one := 1
	c, err := s.CreatePersonal(ctx, "alice", "name", &one)
	if err != nil || c.ExpiresAt.Sub(c.CreatedAt) != 24*time.Hour {
		t.Fatal("custom lifetime failed")
	}
	reads := b.reads
	for _, token := range []string{"", strings.Repeat("x", 513), strings.Replace(c.Token, "v1", "v2", 1), c.Token + "=", strings.ToUpper(c.Token), strings.Replace(c.Token, ".YWxpY2U.", ".YWxpY2U=.", 1)} {
		if _, err := s.AuthenticatePersonal(ctx, token); !errors.Is(err, ErrPersonalInvalid) {
			t.Errorf("invalid format accepted: %v", err)
		}
	}
	if b.reads != reads {
		t.Fatal("malformed token accessed storage")
	}
	parts := strings.Split(c.Token, ".")
	parts[3] = strings.Repeat("A", 43)
	if _, err := s.AuthenticatePersonal(ctx, strings.Join(parts, ".")); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatal("wrong secret accepted")
	}
	s.now = func() time.Time { return c.ExpiresAt }
	if _, err := s.AuthenticatePersonal(ctx, c.Token); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatal("expiry boundary accepted")
	}
}

func TestPersonalCredentialUsesCurrentUser(t *testing.T) {
	s, b := newPersonalStore(t)
	c := personalCreate(t, s)
	for _, spec := range []string{
		`{"enabled":false,"scopes":["ebs:user"]}`, `{"enabled":true,"scopes":["unknown"]}`, `{"enabled":true,"scopes":["ebs:user","ebs:ops"]}`,
	} {
		b.doc.Data = json.RawMessage(`{"metadata":{"name":"alice","uid":"uid-a"},"spec":` + spec + `}`)
		if _, err := s.AuthenticatePersonal(context.Background(), c.Token); !errors.Is(err, ErrPersonalInvalid) {
			t.Fatal("unavailable user accepted")
		}
		if _, err := s.ListPersonal(context.Background(), "alice"); !errors.Is(err, ErrUserUnavailable) {
			t.Fatal("unavailable user list accepted")
		}
	}
	b.doc.Data = json.RawMessage(`{"metadata":{"name":"alice","uid":"uid-a"},"spec":{"enabled":true,"scopes":["ebs:ops"]}}`)
	result, err := s.AuthenticatePersonal(context.Background(), c.Token)
	if err != nil || result.Scopes[0] != "ebs:ops" {
		t.Fatal("current role not used")
	}
	b.doc.Data = json.RawMessage(`{"metadata":{"name":"alice","uid":"uid-a","deletionTimestamp":"2026-11-28T00:00:00Z"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`)
	if _, err := s.AuthenticatePersonal(context.Background(), c.Token); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatal("deleting user accepted")
	}
	b.readStatus = 404
	if _, err := s.AuthenticatePersonal(context.Background(), c.Token); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatal("deleted user accepted")
	}
	b.readStatus = 0
	b.doc.Data = json.RawMessage(`{"metadata":{"name":"alice","uid":"uid-new"},"spec":{"enabled":true,"scopes":["ebs:user"]}}`)
	b.doc.Credential = nil
	if _, err := s.AuthenticatePersonal(context.Background(), c.Token); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatal("recreated user inherited token")
	}
}

func TestPersonalCredentialMissingIndexIsUnavailable(t *testing.T) {
	s, b := newPersonalStore(t)
	c := personalCreate(t, s)
	b.readStatus = 404
	b.readErrorBody = `{"error":{"type":"index_not_found_exception"}}`
	if _, err := s.AuthenticatePersonal(context.Background(), c.Token); err == nil || errors.Is(err, ErrPersonalInvalid) {
		t.Fatal("index failure treated as invalid credential")
	}
	if _, err := s.ListPersonal(context.Background(), "alice"); err == nil || errors.Is(err, ErrUserNotFound) {
		t.Fatal("index failure treated as missing user")
	}
}

func TestPersonalCredentialPreservesPasswordAndIdentity(t *testing.T) {
	s, b := newPersonalStore(t)
	ctx := context.Background()
	c := personalCreate(t, s)
	if err := s.SetPassword(ctx, "alice", "a sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	for range maxFailures {
		if _, err := s.Authenticate(ctx, "alice", "incorrect password"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AuthenticatePersonal(ctx, c.Token); err != nil {
		t.Fatal("password lock affected PAT", err)
	}
	before := b.doc.Credential
	other := personalCreate(t, s)
	if err := s.DeletePersonal(ctx, "alice", other.ID); err != nil {
		t.Fatal(err)
	}
	if string(before) != string(b.doc.Credential) || b.doc.ExternalIdentity.Subject != "123" {
		t.Fatal("PAT mutation changed other credentials")
	}
	if err := s.SetPassword(ctx, "alice", "a new sufficiently long password"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Authenticate(ctx, "alice", "a new sufficiently long password"); err != nil || !ok {
		t.Fatal("password reset failed")
	}
	if _, err := s.AuthenticatePersonal(ctx, c.Token); err != nil {
		t.Fatal("password reset lost PAT", err)
	}
}

func TestPersonalCredentialConflictsAndUnknownWrites(t *testing.T) {
	for _, operation := range []string{"create", "delete"} {
		for _, scenario := range []string{"retry", "exhausted", "replacement", "unavailable", "committed-unknown", "invalid-response"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				s, b := newPersonalStore(t)
				c := personalCreate(t, s)
				b.writes = 0
				switch scenario {
				case "retry":
					b.conflicts = 1
				case "exhausted":
					b.conflicts = 3
				case "replacement":
					b.conflicts = 1
					b.onConflict = func() { b.doc.Data = json.RawMessage(strings.ReplaceAll(string(b.doc.Data), "uid-a", "uid-b")) }
				case "unavailable":
					b.writeStatus = 503
				case "committed-unknown":
					b.writeStatus = 503
					b.commitBeforeFailure = true
				case "invalid-response":
					b.writeStatus = 200
					b.commitBeforeFailure = true
				}
				var err error
				if operation == "delete" {
					err = s.DeletePersonal(context.Background(), "alice", c.ID)
				} else {
					var result *CreatedPersonalCredential
					result, err = s.CreatePersonal(context.Background(), "alice", "new", nil)
					if err != nil && result != nil {
						t.Fatal("unconfirmed token returned")
					}
				}
				wantWrites := 1
				switch scenario {
				case "retry":
					wantWrites = 2
					if err != nil {
						t.Fatal(err)
					}
				case "exhausted":
					wantWrites = 3
					fallthrough
				case "replacement":
					if !errors.Is(err, ErrPersonalConflict) {
						t.Fatalf("expected conflict: %v", err)
					}
				default:
					if err == nil {
						t.Fatal("unknown result treated as success")
					}
				}
				if b.writes != wantWrites {
					t.Fatalf("writes=%d, want %d", b.writes, wantWrites)
				}
			})
		}
	}
}

func TestPersonalCredentialConcurrentCreation(t *testing.T) {
	s, b := newPersonalStore(t)
	for range 4 {
		personalCreate(t, s)
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Independent Store instances share only the ES document, not a local lock.
			replica := NewStore(s.client)
			replica.now = s.now
			_, err := replica.CreatePersonal(context.Background(), "alice", "concurrent", nil)
			if err != nil && !errors.Is(err, ErrPersonalLimit) && !errors.Is(err, ErrPersonalConflict) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	items, err := s.ListPersonal(context.Background(), "alice")
	if err != nil || len(items) != 5 {
		t.Fatalf("concurrent limit: %d %v", len(items), err)
	}
	if b.doc.ExternalIdentity.Subject != "123" {
		t.Fatal("identity overwritten")
	}
}

func TestPersonalCredentialConflictPreservesConcurrentChange(t *testing.T) {
	s, b := newPersonalStore(t)
	revoked := personalCreate(t, s)
	b.conflicts = 1
	b.onConflict = func() {
		b.doc.Credential = json.RawMessage(`{"passwordHash":"concurrent-password","failedAttempts":2}`)
		var user iamv1.User
		_ = json.Unmarshal(b.doc.Data, &user)
		user.Spec.DisplayName = "concurrent"
		b.doc.Data, _ = json.Marshal(user)
	}
	personalCreate(t, s)
	var cred Credential
	_ = json.Unmarshal(b.doc.Credential, &cred)
	if cred.PasswordHash != "concurrent-password" || cred.FailedAttempts != 2 || !strings.Contains(string(b.doc.Data), "concurrent") {
		t.Fatal("concurrent change lost")
	}
	if _, err := s.AuthenticatePersonal(context.Background(), revoked.Token); !errors.Is(err, ErrPersonalInvalid) {
		t.Fatal("concurrently revoked credential restored")
	}
}

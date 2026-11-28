package identity

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParsePersonalToken(t *testing.T) {
	id := strings.Repeat("a", 32)
	secret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	valid := "ebs_pat_v1.YWxpY2U." + id + "." + secret
	name, gotID, err := ParsePersonalToken(valid)
	if err != nil || name != "alice" || gotID != id {
		t.Fatal("valid credential rejected")
	}
	for _, token := range []string{
		"", "ebs_pat_v1", strings.Replace(valid, "v1", "v2", 1), valid + ".extra",
		strings.Replace(valid, "YWxpY2U", "YWxpY2U=", 1), strings.Replace(valid, "YWxpY2U", "Li4vYWxpY2U", 1),
		strings.Replace(valid, id, strings.ToUpper(id), 1), strings.Replace(valid, id, "ab", 1),
		strings.Replace(valid, secret, "AA", 1), valid + "=", strings.Repeat("x", 513),
	} {
		if _, _, err := ParsePersonalToken(token); err == nil {
			t.Error("invalid credential accepted")
		}
	}
}

func TestIssueAndVerify(t *testing.T) {
	tokens, err := NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790000000, 0)
	issued, err := tokens.Issue("alice", UserType, UserScope, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := tokens.Verify(issued, now.Add(time.Minute))
	if err != nil || principal.Type != UserType || principal.Subject != "alice" || principal.Scope != UserScope || principal.ID == "" {
		t.Fatalf("verify issued token: principal=%+v err=%v", principal, err)
	}
	if _, err := tokens.Verify(issued, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired token accepted")
	}
	if _, err := tokens.Verify(issued+"x", now); err == nil {
		t.Fatal("tampered token accepted")
	}
}

func TestRunnerTokenIdentity(t *testing.T) {
	tokens, err := NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Issue("", RunnerType, "", time.Hour, time.Now()); err == nil {
		t.Fatal("empty runner subject accepted")
	}
	if _, err := tokens.Issue("runner-a", RunnerType, UserScope, time.Hour, time.Now()); err == nil {
		t.Fatal("runner with user scope accepted")
	}
	if _, err := tokens.Issue("alice", UserType, "", time.Hour, time.Now()); err == nil {
		t.Fatal("user without a permission scope accepted")
	}
	token, err := tokens.Issue("runner-a", RunnerType, "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	who, err := tokens.Verify(token, time.Now())
	if err != nil || who.Type != RunnerType || who.Subject != "runner-a" || who.Scope != "" {
		t.Fatalf("verify runner token: principal=%+v err=%v", who, err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["runner"]; exists {
		t.Fatal("runner token contains redundant runner claim")
	}
}

func TestServiceTokenIdentity(t *testing.T) {
	tokens, err := NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790000000, 0)
	token, err := tokens.Issue("release-management", ServiceType, "", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	who, err := tokens.Verify(token, now)
	if err != nil || who.Type != ServiceType || who.Subject != "release-management" || who.IsUser() || who.IsPrivileged() || who.ServiceGrants != nil {
		t.Fatalf("principal=%+v err=%v", who, err)
	}
	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var original claims
	if err := json.Unmarshal(payload, &original); err != nil {
		t.Fatal(err)
	}
	if len(original.Scopes) != 0 || original.Scopes == nil {
		t.Fatalf("service claims=%+v", original)
	}
	for _, test := range []struct {
		name   string
		mutate func(*claims)
	}{
		{"empty subject", func(c *claims) { c.Subject = "" }},
		{"user scope", func(c *claims) { c.Scopes = []Scope{AdminScope} }},
		{"empty scope entry", func(c *claims) { c.Scopes = []Scope{""} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := original
			test.mutate(&c)
			data, _ := json.Marshal(c)
			unsigned := parts[0] + "." + base64.RawURLEncoding.EncodeToString(data)
			signed := unsigned + "." + base64.RawURLEncoding.EncodeToString(tokens.signature(unsigned))
			if _, err := tokens.Verify(signed, now); err == nil {
				t.Fatal("invalid service identity accepted")
			}
		})
	}
}

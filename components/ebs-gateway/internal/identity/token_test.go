package identity

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	tokens, err := NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1790000000, 0)
	issued, err := tokens.Issue("alice", "", UserType, UserScope, time.Hour, now)
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

func TestRunnerTypeRequiresMatchingSubject(t *testing.T) {
	tokens, err := NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Issue("runner-a", "runner-b", RunnerType, "", time.Hour, time.Now()); err == nil {
		t.Fatal("mismatched runner accepted")
	}
	if _, err := tokens.Issue("runner-a", "runner-a", RunnerType, UserScope, time.Hour, time.Now()); err == nil {
		t.Fatal("runner with user scope accepted")
	}
	if _, err := tokens.Issue("alice", "", UserType, "", time.Hour, time.Now()); err == nil {
		t.Fatal("user without a permission scope accepted")
	}
	for _, test := range []struct {
		kind   Type
		runner string
	}{
		{RunnerType, "runner-a"},
	} {
		token, err := tokens.Issue("runner-a", test.runner, test.kind, "", time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		who, err := tokens.Verify(token, time.Now())
		if err != nil || who.Type != test.kind || who.Scope != "" {
			t.Fatalf("verify %s token: principal=%+v err=%v", test.kind, who, err)
		}
	}
}

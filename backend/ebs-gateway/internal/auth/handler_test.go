package auth

import (
	"encoding/base64"
	"strings"
	"testing"

	"ebs-gateway/internal/iamclient"
	"ebs-gateway/internal/identity"
	"ebs-gateway/internal/upstream"
)

func TestNew(t *testing.T) {
	client, err := upstream.New("https://api.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := identity.NewTokens(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))
	if err != nil {
		t.Fatal(err)
	}
	deps := Dependencies{IAM: iamclient.New(client), Tokens: tokens, BodyLimit: 1 << 20, LocalAuthEnabled: true}
	handler, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if handler.iam != deps.IAM || handler.tokens != tokens || handler.now == nil || !handler.localAuthEnabled ||
		handler.registerIP == nil || handler.registrationUserLimit == nil || handler.loginIP == nil ||
		handler.loginLimit == nil || handler.machineLimit == nil {
		t.Fatal("authentication dependencies were not initialized")
	}
	for _, invalid := range []Dependencies{
		{Tokens: tokens, BodyLimit: 1},
		{IAM: deps.IAM, BodyLimit: 1},
		{IAM: deps.IAM, Tokens: tokens},
	} {
		if _, err := New(invalid); err == nil {
			t.Fatal("invalid dependencies accepted")
		}
	}
}

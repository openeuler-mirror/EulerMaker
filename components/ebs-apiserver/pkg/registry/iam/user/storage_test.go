package user

import (
	"context"
	"testing"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func objectMeta(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name} }

func TestPrepareForCreateDefaultsEnabled(t *testing.T) {
	user := &iamv1.User{}
	(&strategy{}).PrepareForCreate(context.Background(), user)
	if user.Spec.Enabled == nil || !*user.Spec.Enabled {
		t.Fatal("enabled was not defaulted to true")
	}
}

func TestValidateUser(t *testing.T) {
	tests := []struct {
		name       string
		user       *iamv1.User
		wantErrors bool
	}{
		{name: "valid", user: &iamv1.User{ObjectMeta: objectMeta("alice"), Spec: iamv1.UserSpec{Email: "alice@example.com"}}},
		{name: "invalid name", user: &iamv1.User{ObjectMeta: objectMeta("Alice")}, wantErrors: true},
		{name: "invalid email", user: &iamv1.User{ObjectMeta: objectMeta("alice"), Spec: iamv1.UserSpec{Email: "not-an-email"}}, wantErrors: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(validateUser(tt.user)) > 0; got != tt.wantErrors {
				t.Fatalf("has errors=%v, want %v", got, tt.wantErrors)
			}
		})
	}
}

package machineaccount

import (
	"context"
	"testing"

	iamv1 "ebs-apiserver/pkg/apis/iam/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMachineAccountDefaultsAndValidation(t *testing.T) {
	account := &iamv1.MachineAccount{ObjectMeta: metav1.ObjectMeta{Name: "runner-site-a"}}
	strategy := &strategy{}
	strategy.PrepareForCreate(context.Background(), account)
	if account.Spec.TokenTTLSeconds != 3600 {
		t.Fatalf("default TTL=%d", account.Spec.TokenTTLSeconds)
	}
	if errs := strategy.Validate(context.Background(), account); len(errs) != 0 {
		t.Fatalf("valid account rejected: %v", errs)
	}
	account.Spec.TokenTTLSeconds = 86401
	if errs := strategy.Validate(context.Background(), account); len(errs) == 0 {
		t.Fatal("invalid TTL accepted")
	}
}

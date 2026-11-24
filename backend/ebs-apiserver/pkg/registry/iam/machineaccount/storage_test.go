package machineaccount

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	iamv1 "ebs-api/iam/v1"
)

func TestMachineAccountDefaultsAndValidation(t *testing.T) {
	account := &iamv1.MachineAccount{ObjectMeta: metav1.ObjectMeta{Name: "runner-site-a"}}
	strategy := &strategy{}
	strategy.PrepareForCreate(context.Background(), account)
	if account.Spec.TokenTTLSeconds != 3600 {
		t.Fatalf("default TTL=%d", account.Spec.TokenTTLSeconds)
	}
	if account.Spec.Type != iamv1.MachineAccountRunner {
		t.Fatalf("default type=%q", account.Spec.Type)
	}
	if errs := strategy.Validate(context.Background(), account); len(errs) != 0 {
		t.Fatalf("valid account rejected: %v", errs)
	}
	account.Spec.TokenTTLSeconds = 86401
	if errs := strategy.Validate(context.Background(), account); len(errs) == 0 {
		t.Fatal("invalid TTL accepted")
	}
}

func TestMachineAccountTypeValidation(t *testing.T) {
	s := &strategy{}
	for _, typ := range []iamv1.MachineAccountType{iamv1.MachineAccountRunner, iamv1.MachineAccountService, "", "unknown"} {
		t.Run(string(typ), func(t *testing.T) {
			account := &iamv1.MachineAccount{
				ObjectMeta: metav1.ObjectMeta{Name: "worker"},
				Spec:       iamv1.MachineAccountSpec{Type: typ, TokenTTLSeconds: 3600},
			}
			if errs := s.Validate(context.Background(), account); (len(errs) == 0) != typ.IsValid() {
				t.Fatalf("type=%q errors=%v", typ, errs)
			}
			if !typ.IsValid() {
				return
			}
			updated := account.DeepCopy()
			if errs := s.ValidateUpdate(context.Background(), updated, account); (len(errs) == 0) != (typ == iamv1.MachineAccountService) {
				t.Fatalf("type=%q update errors=%v", typ, errs)
			}
			updated.Spec.Type = iamv1.MachineAccountRunner
			if typ == iamv1.MachineAccountRunner {
				updated.Spec.Type = iamv1.MachineAccountService
			}
			if errs := s.ValidateUpdate(context.Background(), updated, account); len(errs) == 0 {
				t.Fatal("type change accepted")
			}
		})
	}
}

func TestServiceAccountUpdateBoundary(t *testing.T) {
	previous := &iamv1.MachineAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "automation", UID: "id", ResourceVersion: "v1:1:1"},
		Spec:       iamv1.MachineAccountSpec{Type: iamv1.MachineAccountService, TokenTTLSeconds: 3600},
	}
	for _, tc := range []struct {
		name    string
		change  func(*iamv1.MachineAccount)
		allowed bool
	}{
		{"grants", func(a *iamv1.MachineAccount) {
			a.Spec.Projects = []string{"demo"}
			a.Spec.Permissions = []iamv1.MachineAccountPermission{iamv1.MachinePermissionBuildsAbort}
		}, true},
		{"empty grants", func(a *iamv1.MachineAccount) {}, true},
		{"TTL", func(a *iamv1.MachineAccount) { a.Spec.TokenTTLSeconds = 7200 }, false},
		{"labels", func(a *iamv1.MachineAccount) { a.Labels = map[string]string{"x": "y"} }, false},
		{"annotations", func(a *iamv1.MachineAccount) { a.Annotations = map[string]string{"x": "y"} }, false},
		{"finalizer", func(a *iamv1.MachineAccount) { a.Finalizers = []string{"example.com/keep"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := previous.DeepCopy()
			tc.change(next)
			if errs := (&strategy{}).ValidateUpdate(context.Background(), next, previous); (len(errs) == 0) != tc.allowed {
				t.Fatalf("errors=%v", errs)
			}
		})
	}
}

func TestMachineAccountGrantValidationOnCreateAndUpdate(t *testing.T) {
	s := &strategy{}
	account := &iamv1.MachineAccount{
		ObjectMeta: metav1.ObjectMeta{Name: "automation"},
		Spec: iamv1.MachineAccountSpec{
			Type: iamv1.MachineAccountService, TokenTTLSeconds: 3600,
			Projects: []string{"demo"}, Permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionProjectsUpdate},
		},
	}
	if errs := s.Validate(context.Background(), account); len(errs) != 0 {
		t.Fatal(errs)
	}
	invalid := account.DeepCopy()
	invalid.Spec.Permissions = []iamv1.MachineAccountPermission{"admin"}
	for _, errs := range []int{
		len(s.Validate(context.Background(), invalid)),
		len(s.ValidateUpdate(context.Background(), invalid, account)),
	} {
		if errs == 0 {
			t.Fatal("strategy accepted invalid authorization")
		}
	}
}

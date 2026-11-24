package validation

import (
	"strings"
	"testing"

	iamv1 "ebs-api/iam/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateMachineAccountGrants(t *testing.T) {
	for _, test := range []struct {
		name        string
		typ         iamv1.MachineAccountType
		projects    []string
		permissions []iamv1.MachineAccountPermission
		field       string
	}{
		{name: "runner without grants", typ: iamv1.MachineAccountRunner},
		{name: "service without grants", typ: iamv1.MachineAccountService},
		{name: "projects only", typ: iamv1.MachineAccountService, projects: []string{"demo"}},
		{name: "permissions only", typ: iamv1.MachineAccountService, permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionProjectsUpdate}},
		{name: "all permissions", typ: iamv1.MachineAccountService, projects: []string{"demo", "other"}, permissions: []iamv1.MachineAccountPermission{
			iamv1.MachinePermissionProjectsUpdate, iamv1.MachinePermissionBuildsCreate, iamv1.MachinePermissionBuildsAbort,
		}},
		{name: "abort independently", typ: iamv1.MachineAccountService, projects: []string{"demo"}, permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionBuildsAbort}},
		{name: "runner projects", typ: iamv1.MachineAccountRunner, projects: []string{"demo"}, field: "spec.projects"},
		{name: "runner permissions", typ: iamv1.MachineAccountRunner, permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionBuildsCreate}, field: "spec.permissions"},
		{name: "wildcard", typ: iamv1.MachineAccountService, projects: []string{"*"}, field: "spec.projects[0]"},
		{name: "uppercase", typ: iamv1.MachineAccountService, projects: []string{"Demo"}, field: "spec.projects[0]"},
		{name: "long project", typ: iamv1.MachineAccountService, projects: []string{strings.Repeat("a", 64)}, field: "spec.projects[0]"},
		{name: "duplicate project", typ: iamv1.MachineAccountService, projects: []string{"demo", "demo"}, field: "spec.projects[1]"},
		{name: "unknown permission", typ: iamv1.MachineAccountService, permissions: []iamv1.MachineAccountPermission{"builds:delete"}, field: "spec.permissions[0]"},
		{name: "duplicate permission", typ: iamv1.MachineAccountService, permissions: []iamv1.MachineAccountPermission{iamv1.MachinePermissionBuildsCreate, iamv1.MachinePermissionBuildsCreate}, field: "spec.permissions[1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := &iamv1.MachineAccount{
				ObjectMeta: metav1.ObjectMeta{Name: "automation"},
				Spec: iamv1.MachineAccountSpec{
					Type: test.typ, TokenTTLSeconds: 3600, Projects: test.projects, Permissions: test.permissions,
				},
			}
			errs := ValidateMachineAccount(account)
			if test.field == "" {
				if len(errs) != 0 {
					t.Fatal(errs)
				}
			} else if len(errs) != 1 || errs[0].Field != test.field {
				t.Fatalf("errors=%v, want field %s", errs, test.field)
			}
		})
	}
}

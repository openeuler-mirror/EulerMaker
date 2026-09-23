package gateway

import "testing"

func TestIdentityIsPrivileged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
		want   bool
	}{
		{"empty", nil, false},
		{"user", []string{"ebs:user"}, false},
		{"runner", []string{"ebs:runner"}, false},
		{"ops", []string{"ebs:ops"}, true},
		{"admin", []string{"ebs:admin"}, true},
		{"system", []string{"ebs:system"}, true},
		{"unknown", []string{"ebs:unknown"}, false},
		{"prefix", []string{"ebs:ops-extra"}, false},
		{"multiple", []string{"ebs:user", "ebs:ops"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := Identity{Scopes: tc.scopes}
			if got := identity.IsPrivileged(); got != tc.want {
				t.Fatalf("IsPrivileged()=%v, want %v", got, tc.want)
			}
		})
	}
}

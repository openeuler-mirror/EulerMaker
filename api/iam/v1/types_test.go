package v1

import (
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUserRoundTripAndDeepCopy(t *testing.T) {
	enabled := true
	projects := int32(20)
	original := &UserList{Items: []User{{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Labels: map[string]string{"team": "build"}},
		Spec: UserSpec{
			Enabled:     &enabled,
			Scopes:      []string{"ebs:user"},
			DisplayName: "Alice",
			Email:       "alice@example.com",
			Quota:       &UserQuota{Projects: &projects},
		},
	}}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded UserList
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatalf("round trip changed user: %+v", decoded)
	}
	copied := original.DeepCopy()
	*copied.Items[0].Spec.Enabled = false
	copied.Items[0].Spec.Scopes[0] = "ebs:ops"
	*copied.Items[0].Spec.Quota.Projects = 0
	copied.Items[0].Labels["team"] = "other"
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatal("copy modified original user")
	}
}

func TestMachineAccountRoundTripAndDeepCopy(t *testing.T) {
	original := &MachineAccountList{Items: []MachineAccount{{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{"team": "build"}},
		Spec: MachineAccountSpec{
			Type: MachineAccountService, TokenTTLSeconds: 3600,
			Projects: []string{"demo"}, Permissions: []MachineAccountPermission{MachinePermissionProjectsUpdate},
		},
	}}}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded MachineAccountList
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatalf("round trip changed machine account: %+v", decoded)
	}
	copied := original.DeepCopy()
	copied.Items[0].Spec.TokenTTLSeconds = 7200
	copied.Items[0].Spec.Type = MachineAccountRunner
	copied.Items[0].Spec.Projects[0] = "other"
	copied.Items[0].Spec.Permissions[0] = MachinePermissionBuildsAbort
	copied.Items[0].Labels["team"] = "other"
	if !reflect.DeepEqual(original, &decoded) {
		t.Fatal("copy modified original machine account")
	}
}

package v1

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

func TestIAMTypesAreRegistered(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]runtime.Object{
		"User": &User{}, "UserList": &UserList{},
		"MachineAccount": &MachineAccount{}, "MachineAccountList": &MachineAccountList{},
	} {
		obj, err := scheme.New(SchemeGroupVersion.WithKind(kind))
		if err != nil {
			t.Fatal(err)
		}
		if reflect.TypeOf(obj) != reflect.TypeOf(want) {
			t.Fatalf("%s type=%T, want %T", kind, obj, want)
		}
	}
}

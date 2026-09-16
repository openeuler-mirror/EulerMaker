package v1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBuildInfoBootstrapRepo(t *testing.T) {
	if _, ok := reflect.TypeOf(BuildSpec{}).FieldByName("BootstrapRepo"); ok {
		t.Fatal("BuildSpec must not contain BootstrapRepo")
	}
	info := &BuildInfo{Spec: BuildInfoSpec{BootstrapRepo: []BootstrapRepo{{Name: "base", Repo: "https://example.com/repo"}}}}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var decoded BuildInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Spec, info.Spec) {
		t.Fatalf("round trip changed spec: %#v", decoded.Spec)
	}
	copy := info.DeepCopy()
	copy.Spec.BootstrapRepo[0].Repo = "changed"
	if info.Spec.BootstrapRepo[0].Repo != "https://example.com/repo" {
		t.Fatal("DeepCopy shares bootstrap repos")
	}
}

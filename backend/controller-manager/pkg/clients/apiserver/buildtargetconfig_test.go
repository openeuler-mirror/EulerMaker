package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ebsv1 "ebs-api/ebs/v1"
)

func TestBuildTargetConfigBatchSnapshot(t *testing.T) {
	configuration := &ebsv1.BuildTargetContent{Targets: map[string]ebsv1.BuildTargetConfigEntry{
		"os": {Arches: map[string]ebsv1.BuildTargetArch{"x86_64": {Image: "build:x86"}, "aarch64": {Image: "build:arm"}}},
	}}
	calls := 0
	client, err := New(testRESTConfig(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/apis/ebs/v1/configs/build-target" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		content, _ := json.Marshal(configuration)
		data, _ := json.Marshal(&ebsv1.Config{TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Config"}, ObjectMeta: metav1.ObjectMeta{Name: "build-target", UID: "uid", ResourceVersion: "v1:1:1"}, Spec: ebsv1.ConfigSpec{Visibility: ebsv1.ConfigVisibilityPublic, Content: string(content)}})
		return jsonResponse(r, data), nil
	})), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := client.GetBuildTargetContent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	configuration.Targets["os"].Arches["x86_64"] = ebsv1.BuildTargetArch{Image: "build:changed"}
	for arch, want := range map[string]string{"x86_64": "build:x86", "aarch64": "build:arm"} {
		got, err := BuildImage(conf, ebsv1.BuildTarget{Os: "os", Arch: arch})
		if err != nil || got != want {
			t.Fatalf("image=%q error=%v", got, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	if _, err := BuildImage(conf, ebsv1.BuildTarget{Os: "os", Arch: "missing"}); err == nil {
		t.Fatal("accepted missing target")
	}
	if _, err := BuildImage(nil, ebsv1.BuildTarget{}); err == nil {
		t.Fatal("accepted missing config")
	}
}

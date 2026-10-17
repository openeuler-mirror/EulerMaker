package buildinfo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

func TestAPIClientGetBuildResourceContent(t *testing.T) {
	for _, tc := range []struct {
		name, kind, content, wantError string
		status                         int
	}{
		{name: "resource table", kind: "Config", content: "default: {requests: {cpu: '4', memory: 8Gi}}\npackages: {gcc: {default: {requests: {cpu: '8'}}}}"},
		{name: "read failure", status: http.StatusNotFound, wantError: "not found"},
		{name: "response type", kind: "Script", wantError: "response object lacks UID/resourceVersion"},
		{name: "decode failure", kind: "Config", content: "default: [", wantError: "decode build-resource Config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/configs/build-resource" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				obj := &ebsv1.Config{
					TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: tc.kind},
					ObjectMeta: metav1.ObjectMeta{Name: ebsv1.BuildResourceConfigName, UID: "config-uid", ResourceVersion: "1"},
					Spec:       ebsv1.ConfigSpec{Content: tc.content},
				}
				if err := json.NewEncoder(w).Encode(obj); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			shared, err := apiserver.New(&rest.Config{Host: server.URL}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			content, err := newAPIClient(shared).GetBuildResourceContent(context.Background())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v", err)
				}
				if tc.status == http.StatusNotFound && !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing NotFound sentinel: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if content.Default.Requests["cpu"] != "4" || content.Packages["gcc"].Default.Requests["cpu"] != "8" {
				t.Fatalf("unexpected content: %+v", content)
			}
			resolved := resolveResources(content, "gcc", "x86_64")
			if resolved.Requests["cpu"] != "8" || resolved.Limits["cpu"] != "8" || resolved.Requests["memory"] != "8Gi" {
				t.Fatalf("unexpected resources: %+v", resolved)
			}
		})
	}
}

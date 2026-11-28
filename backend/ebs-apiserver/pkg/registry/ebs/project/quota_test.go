package project

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	request "k8s.io/apiserver/pkg/endpoints/request"
	genericregistry "k8s.io/apiserver/pkg/registry/generic/registry"

	ebsv1 "ebs-api/ebs/v1"
	"ebs-apiserver/pkg/storage/es"
	"ebs-apiserver/pkg/storage/esstore"
)

func quotaProject(name string) *ebsv1.Project {
	return &ebsv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{ebsv1.ProjectOwnerUserLabel: "alice"}},
		Spec:       ebsv1.ProjectSpec{Distro: "openEuler", BuildArches: []string{"x86_64"}},
	}
}

func quotaStore(t *testing.T, handler http.HandlerFunc) (*es.Client, *esstore.Store) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := es.NewClientForTesting(server.URL, server.Client())
	store := esstore.New(client, "project", "Project", NewStorage().Resource.(*genericregistry.Store))
	store.SetCreateHook(ValidateQuota(client, store.Count))
	return client, store
}

func TestValidateQuota(t *testing.T) {
	for _, tc := range []struct {
		name          string
		spec          string
		count         int64
		userStatus    int
		countStatus   int
		code          int32
		countRequests int
	}{
		{name: "twentieth project", spec: `{"quota":{"projects":20}}`, count: 19, countRequests: 1},
		{name: "at limit", spec: `{"quota":{"projects":20}}`, count: 20, code: 403, countRequests: 1},
		{name: "already exceeded", spec: `{"quota":{"projects":20}}`, count: 21, code: 403, countRequests: 1},
		{name: "zero quota", spec: `{"quota":{"projects":0}}`, code: 403, countRequests: 1},
		{name: "missing quota", spec: `{}`},
		{name: "missing projects", spec: `{"quota":{}}`},
		{name: "raised quota", spec: `{"quota":{"projects":30}}`, count: 20, countRequests: 1},
		{name: "lowered quota", spec: `{"quota":{"projects":10}}`, count: 20, code: 403, countRequests: 1},
		{name: "owner missing", userStatus: 404, code: 422},
		{name: "user unavailable", userStatus: 503, code: 503},
		{name: "count unavailable", spec: `{"quota":{"projects":20}}`, countStatus: 503, code: 503, countRequests: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var counts atomic.Int32
			client, store := quotaStore(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/ebs-users/_doc/alice":
					want := "data.metadata.name,data.metadata.uid,data.metadata.deletionTimestamp,data.spec.quota"
					if r.URL.Query().Get("_source_includes") != want {
						t.Errorf("unexpected User projection: %s", r.URL.RawQuery)
					}
					if tc.userStatus != 0 {
						w.WriteHeader(tc.userStatus)
						fmt.Fprint(w, `{"found":false}`)
						return
					}
					fmt.Fprintf(w, `{"_source":{"data":{"metadata":{"name":"alice","uid":"alice-uid"},"spec":%s}}}`, tc.spec)
				case r.Method == http.MethodPost && r.URL.Path == "/ebs-projects/_count":
					counts.Add(1)
					var body any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					// Owner key and value must match the same nested label. Do not filter project type or deletion state.
					var want any
					_ = json.Unmarshal([]byte(`{"query":{"bool":{"filter":[{"nested":{"path":"metadata.labels","query":{"bool":{"filter":[{"term":{"metadata.labels.key":"ebs.io/owner-user"}},{"term":{"metadata.labels.value":"alice"}}]}}}}]}}}`), &want)
					if !reflect.DeepEqual(body, want) {
						t.Errorf("count query = %#v, want %#v", body, want)
					}
					if tc.countStatus != 0 {
						w.WriteHeader(tc.countStatus)
						fmt.Fprint(w, `{"error":"unavailable"}`)
						return
					}
					fmt.Fprintf(w, `{"count":%d,"_shards":{"failed":0}}`, tc.count)
				default:
					t.Errorf("unexpected quota request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})
			err := ValidateQuota(client, store.Count)(context.Background(), quotaProject("demo"))
			if tc.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else if status, ok := err.(apierrors.APIStatus); !ok || status.Status().Code != tc.code {
				t.Fatalf("error = %v, want code %d", err, tc.code)
			}
			if tc.code == 403 && !strings.Contains(err.Error(), "ProjectQuotaExceeded: owner=alice") {
				t.Fatalf("quota rejection missing details: %v", err)
			}
			if counts.Load() != int32(tc.countRequests) {
				t.Fatalf("count requests = %d, want %d", counts.Load(), tc.countRequests)
			}
		})
	}
}

func TestQuotaCreateAndDryRun(t *testing.T) {
	var writes atomic.Int32
	_, store := quotaStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/ebs-users/_doc/alice":
			fmt.Fprint(w, `{"_source":{"data":{"metadata":{"name":"alice","uid":"alice-uid"},"spec":{"quota":{"projects":20}}}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/ebs-projects/_count":
			fmt.Fprint(w, `{"count":19,"_shards":{"failed":0}}`)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/ebs-projects/_doc/"):
			if r.URL.Query().Get("op_type") != "create" {
				t.Error("project write is not create-only")
			}
			writes.Add(1)
			fmt.Fprint(w, `{"_seq_no":1,"_primary_term":1}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	ctx := request.WithNamespace(context.Background(), "")
	if _, err := store.Create(ctx, quotaProject("dry-run"), nil, &metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 0 {
		t.Fatal("dry run persisted a project")
	}
	// Both requests may see 19 projects. This soft quota does not reserve the last slot.
	results := make(chan error, 2)
	for _, name := range []string{"demo-a", "demo-b"} {
		go func() {
			_, err := store.Create(ctx, quotaProject(name), nil, &metav1.CreateOptions{})
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if writes.Load() != 2 {
		t.Fatalf("project writes = %d, want 2", writes.Load())
	}
}

func TestQuotaOwnerRequired(t *testing.T) {
	project := quotaProject("demo")
	delete(project.Labels, ebsv1.ProjectOwnerUserLabel)
	if err := ValidateQuota(nil, nil)(context.Background(), project); !apierrors.IsInvalid(err) {
		t.Fatalf("missing owner error = %v", err)
	}
}

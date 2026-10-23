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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	"controller-manager/pkg/clients/apiserver"
	ebsv1 "ebs-api/ebs/v1"
)

func TestAPIClientAbortJob(t *testing.T) {
	for _, tc := range []struct {
		name    string
		uid     string
		phase   ebsv1.JobPhase
		unknown bool
	}{
		{name: "aborted", uid: "job-uid", phase: ebsv1.JobAborted},
		{name: "already succeeded", uid: "job-uid", phase: ebsv1.JobSucceeded},
		{name: "already failed", uid: "job-uid", phase: ebsv1.JobFailed},
		{name: "different incarnation", uid: "new-uid", phase: ebsv1.JobAborted, unknown: true},
		{name: "still running", uid: "job-uid", phase: ebsv1.JobRunning, unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/apis/ebs/v1/projects/demo/jobs/job/abort" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var input map[string]string
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if input["uid"] != "job-uid" || input["reason"] != "parent Build aborted" {
					t.Errorf("abort body = %v", input)
				}
				job := &ebsv1.Job{
					TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Job"},
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "demo", Name: "job", UID: types.UID(tc.uid), ResourceVersion: "2",
					},
					Status: ebsv1.JobStatus{Phase: tc.phase},
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(job); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			shared, err := apiserver.New(&rest.Config{Host: server.URL}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			job, err := newAPIClient(shared).AbortJob(context.Background(), "demo", "job", "job-uid", "parent Build aborted")
			if tc.unknown {
				var writeErr *apiserver.WriteError
				if !errors.As(err, &writeErr) || writeErr.Outcome != apiserver.WriteUnknown {
					t.Fatalf("error = %v, want WriteUnknown", err)
				}
				return
			}
			if err != nil || job == nil || job.Status.Phase != tc.phase || job.UID != "job-uid" {
				t.Fatalf("abort response = %+v, error = %v", job, err)
			}
		})
	}
}

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

func TestAPIClientGetBuildTargetContent(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantError string
		status                   int
	}{
		{name: "yaml", content: "targets: {os: {arches: {x86_64: {image: 'build:x86'}, aarch64: {image: 'build:arm'}}}}"},
		{name: "json", content: `{"targets":{"os":{"arches":{"x86_64":{"image":"build:x86"},"aarch64":{"image":"build:arm"}}}}}`},
		{name: "read failure", status: http.StatusNotFound, wantError: "not found"},
		{name: "decode failure", content: "targets: [", wantError: "decode build-target Config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/configs/build-target" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				config := &ebsv1.Config{
					TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Config"},
					ObjectMeta: metav1.ObjectMeta{
						Name: ebsv1.BuildTargetConfigName, UID: "config-uid", ResourceVersion: "1",
					},
					Spec: ebsv1.ConfigSpec{Content: tc.content},
				}
				if err := json.NewEncoder(w).Encode(config); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			shared, err := apiserver.New(&rest.Config{Host: server.URL}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			content, err := newAPIClient(shared).GetBuildTargetContent(context.Background())
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("GetBuildTargetContent error = %v, want %q", err, tc.wantError)
				}
				if tc.status == http.StatusNotFound && !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing NotFound sentinel: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for arch, want := range map[string]string{"x86_64": "build:x86", "aarch64": "build:arm"} {
				image, err := buildImage(content, ebsv1.BuildTarget{OS: "os", Arch: arch})
				if err != nil || image != want {
					t.Fatalf("image for %s = %q, %v; want %q", arch, image, err, want)
				}
			}
		})
	}
}

func TestAPIClientGetProject(t *testing.T) {
	project := &ebsv1.Project{
		TypeMeta:   metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Project"},
		ObjectMeta: metav1.ObjectMeta{Name: "demo", UID: "project-uid", ResourceVersion: "1"},
		Status:     ebsv1.ProjectStatus{Phase: ebsv1.ProjectTerminating},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/projects/demo" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("includeFields"); got != "metadata,status.phase" {
			t.Errorf("includeFields = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(project); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	shared, err := apiserver.New(&rest.Config{Host: server.URL}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var sharedClient apiserver.Interface = shared
	got, err := newAPIClient(sharedClient).GetProject(context.Background(), project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != project.Name || got.UID != project.UID || got.ResourceVersion != project.ResourceVersion ||
		got.Status.Phase != ebsv1.ProjectTerminating {
		t.Fatalf("unexpected Project identity/status: %+v", got)
	}
}

func TestAPIClientGetSnapshot(t *testing.T) {
	for _, fields := range []string{"", "metadata,spec.packageRepos.name"} {
		t.Run("fields="+fields, func(t *testing.T) {
			snapshot := &ebsv1.Snapshot{
				TypeMeta: metav1.TypeMeta{APIVersion: "ebs/v1", Kind: "Snapshot"},
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "demo", Name: "build", UID: "snapshot-uid", ResourceVersion: "1",
				},
				Spec: ebsv1.SnapshotSpec{PackageRepos: []ebsv1.PackageRepo{{Name: "gcc"}}},
			}
			if fields == "" {
				snapshot.Spec.PackageRepos[0].URL = "https://example.test/gcc.git"
				snapshot.Status.Phase = ebsv1.SnapshotActive
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/apis/ebs/v1/projects/demo/snapshots/build" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if got := r.URL.Query().Get("includeFields"); got != fields {
					t.Errorf("includeFields = %q, want %q", got, fields)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(snapshot); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			shared, err := apiserver.New(&rest.Config{Host: server.URL}, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client := newAPIClient(shared)
			var selection []string
			if fields != "" {
				selection = []string{fields}
			}
			got, err := client.GetSnapshot(context.Background(), "demo", "build", selection...)
			if err != nil {
				t.Fatal(err)
			}
			if got.UID != snapshot.UID || got.ResourceVersion != snapshot.ResourceVersion ||
				got.Status.Phase != snapshot.Status.Phase || len(got.Spec.PackageRepos) != 1 ||
				got.Spec.PackageRepos[0] != snapshot.Spec.PackageRepos[0] {
				t.Fatalf("unexpected Snapshot: %+v", got)
			}
		})
	}
}

func TestAPIClientBuildGraph(t *testing.T) {
	info := testBuildInfoObj(ebsv1.BuildInfoPending)
	info.UID = "info-uid"
	stored := newBuildGraph(info, dcgStateAB())
	stored.UID, stored.ResourceVersion = "graph-uid", "1"
	path := "/apis/ebs/v1/projects/" + info.Namespace + "/buildgraphs"
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		response := stored.DeepCopy()
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("includeFields") == "metadata" {
				response.Status = ebsv1.BuildGraphStatus{}
			}
		case http.MethodPost, http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(response); err != nil {
				t.Error(err)
			}
			response.UID, response.ResourceVersion = stored.UID, "2"
			stored = response.DeepCopy()
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	shared, err := apiserver.New(&rest.Config{Host: server.URL}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := newAPIClient(shared)
	ctx := context.Background()
	meta, err := client.GetBuildGraph(ctx, info.Namespace, info.Name, true)
	if err != nil || meta == nil || meta.UID != stored.UID || meta.Status.Nodes != nil {
		t.Fatalf("metadata read: %v %v", meta, err)
	}
	graph, err := client.GetBuildGraph(ctx, info.Namespace, info.Name, false)
	if err != nil || graph == nil || !dcgStateMatches(graph.Status.Nodes, stored.Status.Nodes) {
		t.Fatalf("full read: %v", err)
	}
	graph, err = client.CreateBuildGraph(ctx, info.Namespace, newBuildGraph(info, dcgStateAB()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UpdateBuildGraphStatus(ctx, graph); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET " + path + "/" + info.Name + "?includeFields=metadata",
		"GET " + path + "/" + info.Name,
		"POST " + path,
		"PUT " + path + "/" + info.Name + "/status",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests: %v", requests)
	}
}

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ebs-runner/pkg/gateway"
	"ebs-runner/pkg/options"
)

func TestCancelledExecutionRetainsResumableData(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		root := t.TempDir()
		job := gateway.JobResource{
			Metadata: gateway.ObjectMeta{Namespace: "project", Name: "job", UID: "uid"},
			Status:   gateway.JobStatus{Phase: "Running", Stage: "PostRun"},
		}
		api := &workCleanupAPI{jobs: map[string]gateway.JobResource{"project/job": job}}
		if unavailable {
			api.err = errors.New("gateway unavailable")
		}
		a := &Agent{cfg: options.Config{RootDir: root}, client: api}
		_, key, _ := a.registerExecution(context.Background(), job)
		for _, category := range []string{"work", "results", "logs", "uploads"} {
			if err := os.MkdirAll(filepath.Join(root, category, "project", "job"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		a.cancelExecution(key)
		a.finishJob(key)
		for _, category := range []string{"work", "results", "logs", "uploads"} {
			if _, err := os.Stat(filepath.Join(root, category, "project", "job")); err != nil {
				t.Fatalf("resumable %s removed (unavailable=%t): %v", category, unavailable, err)
			}
		}
	}
}

func TestAgentVersionStatus(t *testing.T) {
	a := &Agent{cfg: options.Config{RootDir: t.TempDir()}}
	status := a.currentStatus()
	if status.Info.AgentVersion != agentVersion {
		t.Fatalf("agentVersion = %q, want %q", status.Info.AgentVersion, agentVersion)
	}
	t.Logf("reported agentVersion: %s", status.Info.AgentVersion)
}

func TestMemoryQuantityFromMeminfo(t *testing.T) {
	meminfo := "MemTotal:       65857640 kB\nMemFree:         1024 kB\n"
	got := memoryQuantityFromMeminfo(meminfo)
	if got != "64314Mi" {
		t.Fatalf("expected 64314Mi, got %s", got)
	}
}

func TestBytesToGiQuantity(t *testing.T) {
	got := bytesToGiQuantity(10 * 1024 * 1024 * 1024)
	if got != "10Gi" {
		t.Fatalf("expected 10Gi, got %s", got)
	}
}

const testRunnerInstanceID = "01234567-89ab-4def-8123-456789abcdef"

type registrationAPI struct {
	getResults []*gateway.RunnerResource
	getErrors  []error
	getCalls   int
	createErr  error
	created    *gateway.RunnerResource
	updated    *gateway.RunnerResource
}

func (f *registrationAPI) GetRunner(context.Context, string) (*gateway.RunnerResource, error) {
	index := f.getCalls
	f.getCalls++
	var result *gateway.RunnerResource
	var err error
	if index < len(f.getResults) {
		result = f.getResults[index]
	}
	if index < len(f.getErrors) {
		err = f.getErrors[index]
	}
	return result, err
}

func (f *registrationAPI) CreateRunner(_ context.Context, runner gateway.RunnerResource) error {
	f.created = &runner
	return f.createErr
}

func (f *registrationAPI) UpdateRunner(_ context.Context, runner gateway.RunnerResource) error {
	f.updated = &runner
	return nil
}

func (*registrationAPI) PatchRunnerStatus(context.Context, string, gateway.RunnerStatus) error {
	return nil
}

func (*registrationAPI) GetJob(context.Context, string, string) (*gateway.JobResource, error) {
	return nil, os.ErrNotExist
}

func (*registrationAPI) UpdateJobStatus(_ context.Context, job gateway.JobResource, status gateway.JobStatus) (*gateway.JobResource, error) {
	job.Status = status
	return &job, nil
}

func (*registrationAPI) ListAssignedJobs(context.Context, string) (*gateway.JobList, error) {
	return nil, nil
}

func (*registrationAPI) WatchAssignedJobs(context.Context, string, string) (<-chan gateway.WatchEvent, <-chan error) {
	return nil, nil
}

func newRegistrationAgent(client RunnerAPI) *Agent {
	return &Agent{
		cfg:    options.Config{Name: "runner-a", Type: "ct", Arch: "x86_64"},
		client: client, instanceID: testRunnerInstanceID,
	}
}

func TestRegisterCreatesRunnerWithInstanceID(t *testing.T) {
	client := &registrationAPI{getErrors: []error{gateway.StatusError{Code: 404}}}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if client.created == nil || client.created.Spec.InstanceID != testRunnerInstanceID {
		t.Fatalf("created runner = %#v", client.created)
	}
}

func TestRegisterUpdatesMatchingInstance(t *testing.T) {
	existing := &gateway.RunnerResource{
		Metadata: gateway.ObjectMeta{Name: "runner-a", Labels: map[string]string{"custom": "value"}},
		Spec:     gateway.RunnerSpec{InstanceID: testRunnerInstanceID, Type: "vm", Arch: "aarch64", Unschedulable: true},
	}
	client := &registrationAPI{getResults: []*gateway.RunnerResource{existing}}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if client.updated == nil || client.updated.Spec.InstanceID != testRunnerInstanceID || !client.updated.Spec.Unschedulable {
		t.Fatalf("updated runner = %#v", client.updated)
	}
	if client.updated.Metadata.Labels["custom"] != "value" {
		t.Fatalf("custom labels were not preserved: %#v", client.updated.Metadata.Labels)
	}
}

func TestRegisterRejectsDifferentInstance(t *testing.T) {
	existing := &gateway.RunnerResource{Spec: gateway.RunnerSpec{InstanceID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}}
	client := &registrationAPI{getResults: []*gateway.RunnerResource{existing}}
	err := newRegistrationAgent(client).register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different instance") {
		t.Fatalf("expected instance conflict, got %v", err)
	}
	if client.updated != nil {
		t.Fatal("different instance was updated")
	}
}

func TestRegisterResolvesCreateConflictByInstanceID(t *testing.T) {
	existing := &gateway.RunnerResource{Spec: gateway.RunnerSpec{InstanceID: testRunnerInstanceID}}
	client := &registrationAPI{
		getResults: []*gateway.RunnerResource{nil, existing},
		getErrors:  []error{gateway.StatusError{Code: 404}, nil},
		createErr:  gateway.StatusError{Code: 409},
	}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register after matching conflict: %v", err)
	}
	if client.updated == nil {
		t.Fatal("matching runner was not reconciled after create conflict")
	}
}

func TestReadJobResult(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		wantErr             bool
	}{
		{name: "absent"},
		{name: "success", content: `{"status":"Succeeded"}`, want: "Succeeded"},
		{name: "build failure", content: `{"status":"BuildFailed"}`, want: "BuildFailed"},
		{name: "install failure", content: `{"status":"InstallFailed","missingDeps":{"foo":{"neededBy":"bar"}}}`, want: "InstallFailed"},
		{name: "dependency scope", content: `{"status":"BuildFailed","missingDeps":{"foo":{}}}`, wantErr: true},
		{name: "incomplete result", content: `{}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "job-result.json")
			if tc.content != "" {
				if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := readJobResult(path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("read result = %+v, %v", result, err)
			}
			if tc.want != "" && (result == nil || result.Status != tc.want) {
				t.Fatalf("result = %+v, want %s", result, tc.want)
			}
			if tc.name == "absent" && result != nil {
				t.Fatalf("absent result inferred as %+v", result)
			}
		})
	}
}

package runner

import (
	"context"
	"os"
	"strings"
	"testing"
)

const testRunnerInstanceID = "01234567-89ab-4def-8123-456789abcdef"

type registrationAPI struct {
	getResults []*RunnerResource
	getErrors  []error
	getCalls   int
	createErr  error
	created    *RunnerResource
	updated    *RunnerResource
}

func (f *registrationAPI) GetRunner(context.Context, string) (*RunnerResource, error) {
	index := f.getCalls
	f.getCalls++
	var result *RunnerResource
	var err error
	if index < len(f.getResults) {
		result = f.getResults[index]
	}
	if index < len(f.getErrors) {
		err = f.getErrors[index]
	}
	return result, err
}

func (f *registrationAPI) CreateRunner(_ context.Context, runner RunnerResource) error {
	f.created = &runner
	return f.createErr
}

func (f *registrationAPI) UpdateRunner(_ context.Context, runner RunnerResource) error {
	f.updated = &runner
	return nil
}

func (*registrationAPI) PatchRunnerStatus(context.Context, string, RunnerStatus) error { return nil }
func (*registrationAPI) GetJob(context.Context, string, string) (*JobResource, error) {
	return nil, os.ErrNotExist
}
func (*registrationAPI) UpdateJobStatus(_ context.Context, job JobResource, status JobStatus) (*JobResource, error) {
	job.Status = status
	return &job, nil
}
func (*registrationAPI) ListAssignedJobs(context.Context, string) (*JobList, error) { return nil, nil }
func (*registrationAPI) WatchAssignedJobs(context.Context, string, string) (<-chan WatchEvent, <-chan error) {
	return nil, nil
}

func newRegistrationAgent(client RunnerAPI) *Agent {
	return &Agent{
		cfg:    Config{Name: "runner-a", Type: "ct", Arch: "x86_64"},
		client: client, instanceID: testRunnerInstanceID,
	}
}

func TestRegisterCreatesRunnerWithInstanceID(t *testing.T) {
	client := &registrationAPI{getErrors: []error{StatusError{Code: 404}}}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if client.created == nil || client.created.Spec.InstanceID != testRunnerInstanceID {
		t.Fatalf("created runner = %#v", client.created)
	}
}

func TestRegisterUpdatesMatchingInstance(t *testing.T) {
	existing := &RunnerResource{
		Metadata: ObjectMeta{Name: "runner-a", Labels: map[string]string{"custom": "value"}},
		Spec:     RunnerSpec{InstanceID: testRunnerInstanceID, Type: "vm", Arch: "aarch64", Unschedulable: true},
	}
	client := &registrationAPI{getResults: []*RunnerResource{existing}}
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
	existing := &RunnerResource{Spec: RunnerSpec{InstanceID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}}
	client := &registrationAPI{getResults: []*RunnerResource{existing}}
	err := newRegistrationAgent(client).register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different instance") {
		t.Fatalf("expected instance conflict, got %v", err)
	}
	if client.updated != nil {
		t.Fatal("different instance was updated")
	}
}

func TestRegisterResolvesCreateConflictByInstanceID(t *testing.T) {
	existing := &RunnerResource{Spec: RunnerSpec{InstanceID: testRunnerInstanceID}}
	client := &registrationAPI{
		getResults: []*RunnerResource{nil, existing},
		getErrors:  []error{StatusError{Code: 404}, nil},
		createErr:  StatusError{Code: 409},
	}
	if err := newRegistrationAgent(client).register(context.Background()); err != nil {
		t.Fatalf("register after matching conflict: %v", err)
	}
	if client.updated == nil {
		t.Fatal("matching runner was not reconciled after create conflict")
	}
}

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCTExecutorCreatesContainerWithPayloadFile(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 0}
	runtimeSpec := mustJSON(t, ContainerRuntimeSpec{
		Image:       "openeuler:22.03",
		NetworkMode: "bridge",
		WorkingDir:  "/workspace",
		Env: map[string]string{
			"BUILD_ENV": "production",
		},
		Mounts: []ContainerMount{
			{Name: "work", MountPath: "/workspace"},
			{Name: "results", MountPath: "/results"},
		},
	})
	executor := &CTExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		RunnerName: "runner-a",
		Runtime:    container,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec: JobSpec{
			RuntimeSpec: runtimeSpec,
			Payload:     "build:\n  target: rpm\n",
		},
	}

	resultRoot, err := executor.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resultRoot != filepath.Join(dir, "results", "project-a", "job-a") {
		t.Fatalf("result root = %s", resultRoot)
	}
	payload, err := os.ReadFile(filepath.Join(dir, "work", "project-a", "job-a", "payload.yaml"))
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(payload) != "build:\n  target: rpm\n" {
		t.Fatalf("payload = %q", string(payload))
	}
	if container.created.Image != "openeuler:22.03" {
		t.Fatalf("image = %q", container.created.Image)
	}
	if container.created.Entrypoint != "" {
		t.Fatalf("empty scriptRefs overrode image entrypoint: %q", container.created.Entrypoint)
	}
	if container.created.Labels["ebs.io/project"] != "project-a" || container.created.Labels["ebs.io/job"] != "job-a" || container.created.Labels["ebs.io/runner"] != "runner-a" {
		t.Fatalf("labels = %#v", container.created.Labels)
	}
	if container.created.Mounts[filepath.Join(dir, "work", "project-a", "job-a")] != "/workspace" {
		t.Fatalf("work mount = %#v", container.created.Mounts)
	}
	if container.created.Mounts[filepath.Join(dir, "results", "project-a", "job-a")] != "/results" {
		t.Fatalf("result mount = %#v", container.created.Mounts)
	}
	if !container.started || !container.removed {
		t.Fatalf("expected container start and cleanup, started=%v removed=%v", container.started, container.removed)
	}
	logData, err := os.ReadFile(filepath.Join(resultRoot, "container.log"))
	if err != nil {
		t.Fatalf("read container log: %v", err)
	}
	if string(logData) != "container log\n" {
		t.Fatalf("container log = %q", string(logData))
	}
}

func TestCTExecutorUsesJobScript(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 0}
	source := &fakeScriptSource{script: testScript("rpmbuild", "uid-1", "rv-1", "#!/bin/sh\necho built\n")}
	executor := &CTExecutor{
		WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
		Runtime: container, Scripts: NewScriptCache(source),
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec: JobSpec{
			RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
			ScriptRefs:  []ScriptRef{{Name: "rpmbuild", UID: "uid-1", ResourceVersion: "rv-1"}},
		},
	}
	if _, err := executor.Execute(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if container.created.Entrypoint != "/workspace/scripts/rpmbuild" || len(container.created.Command) != 0 || len(container.created.Args) != 0 {
		t.Fatalf("container entrypoint = %q, command=%v, args=%v", container.created.Entrypoint, container.created.Command, container.created.Args)
	}
	path := filepath.Join(dir, "work", "project-a", "job-a", "scripts", "rpmbuild")
	content, err := os.ReadFile(path)
	if err != nil || string(content) != source.script.Spec.Content {
		t.Fatalf("script file = %q, %v", content, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o555 {
		t.Fatalf("script mode = %v", info.Mode())
	}
}

func TestCTExecutorRejectsScriptAndRuntimeCommand(t *testing.T) {
	executor := &CTExecutor{WorkDir: t.TempDir(), ResultRoot: t.TempDir()}
	job := JobResource{Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"}, Spec: JobSpec{
		RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03", Command: []string{"echo"}}),
		ScriptRefs:  []ScriptRef{{Name: "rpmbuild", UID: "uid-1", ResourceVersion: "rv-1"}},
	}}
	if _, err := executor.Execute(context.Background(), job); err == nil {
		t.Fatal("runtime command accepted with scriptRef")
	}
}

func TestCTExecutorUsesFirstScriptAsEntrypoint(t *testing.T) {
	dir := t.TempDir()
	source := &fakeMultiScriptSource{scripts: map[string]ScriptResource{
		"first":  testScript("first", "uid-1", "rv-1", "#!/bin/sh\necho first\n"),
		"second": testScript("second", "uid-2", "rv-2", "#!/bin/sh\necho second\n"),
	}}
	container := &fakeContainerRuntime{exitCode: 0}
	executor := &CTExecutor{WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"), Runtime: container, Scripts: NewScriptCache(source)}
	job := JobResource{Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"}, Spec: JobSpec{
		RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
		ScriptRefs: []ScriptRef{
			{Name: "first", UID: "uid-1", ResourceVersion: "rv-1"},
			{Name: "second", UID: "uid-2", ResourceVersion: "rv-2"},
		},
	}}
	if _, err := executor.Execute(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if container.created.Entrypoint != "/workspace/scripts/first" {
		t.Fatalf("entrypoint = %q, want first script", container.created.Entrypoint)
	}
	for name, script := range source.scripts {
		content, err := os.ReadFile(filepath.Join(dir, "work", "project-a", "job-a", "scripts", name))
		if err != nil || string(content) != script.Spec.Content {
			t.Fatalf("script %s = %q, %v", name, content, err)
		}
	}
	if len(source.calls) != 2 || source.calls[0] != "first" || source.calls[1] != "second" {
		t.Fatalf("script read order = %v", source.calls)
	}
}

func TestCTExecutorRejectsDuplicateOrUnsafeScriptNames(t *testing.T) {
	for _, names := range [][]string{{"rpmbuild", "rpmbuild"}, {"../outside"}} {
		executor := &CTExecutor{WorkDir: t.TempDir(), ResultRoot: t.TempDir()}
		refs := make([]ScriptRef, 0, len(names))
		for _, name := range names {
			refs = append(refs, ScriptRef{Name: name, UID: "uid-1", ResourceVersion: "rv-1"})
		}
		job := JobResource{Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"}, Spec: JobSpec{
			RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"}),
			ScriptRefs:  refs,
		}}
		if _, err := executor.Execute(context.Background(), job); err == nil {
			t.Fatalf("script names %v accepted", names)
		}
	}
}

type fakeMultiScriptSource struct {
	scripts map[string]ScriptResource
	calls   []string
}

func (f *fakeMultiScriptSource) GetScript(_ context.Context, name string) (*ScriptResource, error) {
	f.calls = append(f.calls, name)
	script, ok := f.scripts[name]
	if !ok {
		return nil, errors.New("Script not found")
	}
	return &script, nil
}

func TestCTExecutorReturnsContainerExitCode(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 7}
	executor := &CTExecutor{
		WorkDir:    filepath.Join(dir, "work"),
		ResultRoot: filepath.Join(dir, "results"),
		Runtime:    container,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})},
	}

	_, err := executor.Execute(context.Background(), job)
	if err == nil || err.Error() != "container exited with code 7" {
		t.Fatalf("expected exit code error, got %v", err)
	}
}

func TestCTExecutorCompletesRealtimeLogWhenContainerFails(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{exitCode: 7}
	sink := &recordingLogSink{}
	executor := &CTExecutor{
		WorkDir: filepath.Join(dir, "work"), ResultRoot: filepath.Join(dir, "results"),
		Runtime: container, LogFactory: staticLogFactory{sink: sink}, LogDrainTimeout: time.Second,
	}
	job := JobResource{Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a", UID: "uid-a"}, Spec: JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})}}
	resultRoot, err := executor.Execute(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "container exited with code 7") {
		t.Fatalf("error = %v", err)
	}
	if resultRoot != filepath.Join(dir, "results", "project-a", "job-a") {
		t.Fatalf("result root = %s", resultRoot)
	}
	if string(sink.data) != "container log\n" || !sink.completed {
		t.Fatalf("sink data=%q completed=%v", sink.data, sink.completed)
	}
}

func TestCTExecutorDoesNotCreateContainerAfterContextCancel(t *testing.T) {
	dir := t.TempDir()
	container := &fakeContainerRuntime{waitBlock: make(chan struct{})}
	executor := &CTExecutor{
		WorkDir:         filepath.Join(dir, "work"),
		ResultRoot:      filepath.Join(dir, "results"),
		Runtime:         container,
		StopGracePeriod: time.Millisecond,
	}
	job := JobResource{
		Metadata: ObjectMeta{Name: "job-a", Namespace: "project-a"},
		Spec:     JobSpec{RuntimeSpec: mustJSON(t, ContainerRuntimeSpec{Image: "openeuler:22.03"})},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.Execute(ctx, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if container.started || container.created.Image != "" {
		t.Fatalf("cancelled execution created a container")
	}
}

func TestWaitContainerStopsOnCancellation(t *testing.T) {
	container := &fakeContainerRuntime{waitBlock: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitContainer(ctx, container, "id", time.Millisecond)
	if !errors.Is(err, context.Canceled) || !container.stopped {
		t.Fatalf("stop=%v err=%v", container.stopped, err)
	}
}

func TestRuntimeManagerDispatchesCT(t *testing.T) {
	executor := &fakeExecutor{resultRoot: "/results/project/job"}
	manager := &RuntimeManager{
		RunnerType: "ct",
		Executors:  map[string]Executor{"ct": executor},
	}

	resultRoot, err := manager.Execute(context.Background(), JobResource{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resultRoot != "/results/project/job" {
		t.Fatalf("result root = %s", resultRoot)
	}
	if !executor.called {
		t.Fatalf("expected executor call")
	}
}

func TestRuntimeManagerRejectsMismatchedRuntime(t *testing.T) {
	manager := &RuntimeManager{
		RunnerType: "ct",
		Executors:  map[string]Executor{"ct": &fakeExecutor{}},
	}

	_, err := manager.Execute(context.Background(), JobResource{Spec: JobSpec{Runtime: "vm"}})
	if err == nil {
		t.Fatalf("expected runtime mismatch error")
	}
}

type fakeContainerRuntime struct {
	mu        sync.Mutex
	created   ContainerSpec
	started   bool
	stopped   bool
	removed   bool
	pulled    bool
	exitCode  int
	waitBlock chan struct{}
}

type staticLogFactory struct{ sink JobLogSink }

func (f staticLogFactory) Open(JobResource) (JobLogSink, error) { return f.sink, nil }

type recordingLogSink struct {
	data      []byte
	completed bool
}

func (s *recordingLogSink) Write(p []byte) (int, error) {
	s.data = append(s.data, p...)
	return len(p), nil
}
func (s *recordingLogSink) Complete(context.Context) (CompletedLog, error) {
	s.completed = true
	return CompletedLog{State: "Completed"}, nil
}
func (s *recordingLogSink) Abort() {}

func (f *fakeContainerRuntime) ImageExists(context.Context, string) (bool, error) {
	return !f.pulled, nil
}

func (f *fakeContainerRuntime) Pull(context.Context, string) error {
	f.pulled = true
	return nil
}

func (f *fakeContainerRuntime) Remove(context.Context, string) error {
	f.removed = true
	return nil
}

func (f *fakeContainerRuntime) Create(_ context.Context, spec ContainerSpec) (string, error) {
	f.created = spec
	return "container-a", nil
}

func (f *fakeContainerRuntime) Start(context.Context, string) error {
	f.started = true
	return nil
}

func (f *fakeContainerRuntime) Logs(ctx context.Context, _ string, output io.Writer) error {
	_, _ = io.WriteString(output, "container log\n")
	return nil
}

func (f *fakeContainerRuntime) Wait(context.Context, string) (int, error) {
	f.mu.Lock()
	waitBlock := f.waitBlock
	f.mu.Unlock()
	if waitBlock != nil {
		<-waitBlock
	}
	return f.exitCode, nil
}

func (f *fakeContainerRuntime) Stop(context.Context, string, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	if f.waitBlock != nil {
		close(f.waitBlock)
		f.waitBlock = nil
	}
	return nil
}

func (f *fakeContainerRuntime) Kill(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.waitBlock != nil {
		close(f.waitBlock)
		f.waitBlock = nil
	}
	return nil
}

type fakeExecutor struct {
	called     bool
	resultRoot string
	err        error
}

func (f *fakeExecutor) Execute(context.Context, JobResource) (string, error) {
	f.called = true
	return f.resultRoot, f.err
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}

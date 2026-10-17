package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/filters"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

type configImage struct {
	store content.Store
	desc  imagespec.Descriptor
}

func (i configImage) Config(context.Context) (imagespec.Descriptor, error) { return i.desc, nil }
func (i configImage) ContentStore() content.Store                          { return i.store }

func TestContainerdSpec(t *testing.T) {
	store, err := local.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(imagespec.Image{
		Platform: imagespec.Platform{OS: "linux", Architecture: "amd64"},
		Config: imagespec.ImageConfig{Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", "echo image"},
			Env: []string{"PATH=/usr/bin", "VALUE=image"}, WorkingDir: "/image"},
	})
	if err != nil {
		t.Fatal(err)
	}
	desc := imagespec.Descriptor{MediaType: imagespec.MediaTypeImageConfig, Digest: digest.FromBytes(body), Size: int64(len(body))}
	if err := content.WriteBlob(context.Background(), store, "config", bytes.NewReader(body), desc); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		spec Spec
		args []string
	}{
		{"image defaults", Spec{}, []string{"/bin/sh", "-c", "echo image"}},
		{"command override", Spec{Command: []string{"-c"}, Args: []string{"echo job"}}, []string{"/bin/sh", "-c", "echo job"}},
		{"script entrypoint", Spec{Entrypoint: "/workspace/scripts/rpmbuild"}, []string{"/workspace/scripts/rpmbuild"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.spec.Env = map[string]string{"VALUE": "job"}
			tt.spec.Mounts = map[string]string{"/srv/work": "/workspace", "/srv/sub": "/workspace/sub"}
			opts, err := containerdSpecOptions(configImage{store, desc}, tt.spec)
			if err != nil {
				t.Fatal(err)
			}
			opts = append([]oci.SpecOpts{oci.WithRootFSPath(t.TempDir())}, opts...)
			ctx := namespaces.WithNamespace(context.Background(), "eulermaker")
			spec, err := oci.GenerateSpec(ctx, nil, &containers.Container{ID: "test"}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(spec.Process.Args, tt.args) || spec.Process.Cwd != "/image" {
				t.Fatalf("process = %+v", spec.Process)
			}
			if !slices.Contains(spec.Process.Env, "VALUE=job") || !slices.Contains(spec.Process.Env, "PATH=/usr/bin") {
				t.Fatalf("env = %v", spec.Process.Env)
			}
			for _, ns := range spec.Linux.Namespaces {
				if ns.Type == specs.NetworkNamespace {
					t.Fatal("host network should share the host namespace")
				}
			}
			mounts := spec.Mounts[len(spec.Mounts)-2:]
			if mounts[0].Source != "/srv/work" || mounts[1].Destination != "/workspace/sub" {
				t.Fatalf("bind mount ordering = %v", mounts)
			}
		})
	}
}

func TestContainerdImageReference(t *testing.T) {
	for input, want := range map[string]string{
		"busybox": "docker.io/library/busybox:latest",
		"swr.cn-north-4.myhuaweicloud.com/eulermaker/build:dev": "swr.cn-north-4.myhuaweicloud.com/eulermaker/build:dev",
	} {
		got, err := imageReference(input)
		if err != nil || got != want {
			t.Fatalf("imageReference(%q) = %q, %v", input, got, err)
		}
	}
}

func TestContainerdLogsFollowAndDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.log")
	writer, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	reader, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	execution := &containerdExecution{done: make(chan struct{})}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- followContainerdLog(ctx, reader, &output, execution) }()
	for _, line := range []string{"stdout\n", "stderr\n", "final bytes without newline"} {
		if _, err := writer.WriteString(line); err != nil {
			t.Fatal(err)
		}
	}
	close(execution.done)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if output.String() != "stdout\nstderr\nfinal bytes without newline" {
		t.Fatalf("logs = %q", output.String())
	}
}

func TestContainerdWaitCancellation(t *testing.T) {
	r := &Containerd{tasks: map[string]*containerdExecution{"job": {done: make(chan struct{})}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Wait(ctx, "job"); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v", err)
	}
	r.tasks["job"].code = 7
	close(r.tasks["job"].done)
	if code, err := r.Wait(context.Background(), "job"); err != nil || code != 7 {
		t.Fatalf("wait = %d, %v", code, err)
	}
}

type runtimeStore struct {
	containers.Store
	record  containers.Container
	deleted bool
}

func (s *runtimeStore) Get(ctx context.Context, id string) (containers.Container, error) {
	if _, err := namespaces.NamespaceRequired(ctx); err != nil {
		return containers.Container{}, err
	}
	if s.deleted {
		return containers.Container{}, errdefs.ErrNotFound
	}
	return s.record, nil
}

func (s *runtimeStore) List(ctx context.Context, expressions ...string) ([]containers.Container, error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil || namespace != "eulermaker" {
		return nil, errors.New("missing execution namespace")
	}
	filter, err := filters.ParseAll(expressions...)
	if err != nil {
		return nil, err
	}
	if filter.Match(filters.AdapterFunc(func(path []string) (string, bool) {
		if len(path) != 2 || path[0] != "labels" {
			return "", false
		}
		value, found := s.record.Labels[path[1]]
		return value, found
	})) {
		return []containers.Container{s.record}, nil
	}
	return nil, nil
}

func (s *runtimeStore) Delete(context.Context, string) error {
	s.deleted = true
	return nil
}

type runtimeSnapshots struct {
	snapshots.Snapshotter
	removed string
}

func (*runtimeSnapshots) Mounts(context.Context, string) ([]mount.Mount, error) { return nil, nil }
func (s *runtimeSnapshots) Remove(_ context.Context, key string) error {
	s.removed = key
	return nil
}

type runtimeTasks struct {
	tasks.TasksClient
	mu           sync.Mutex
	status       tasktypes.Status
	exited       chan struct{}
	path         string
	signals      []uint32
	deleted      bool
	exitOnStart  bool
	startFailure error
}

func (s *runtimeTasks) Create(_ context.Context, req *tasks.CreateTaskRequest, _ ...grpc.CallOption) (*tasks.CreateTaskResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, err := url.Parse(req.Stdout)
	if err != nil {
		return nil, err
	}
	s.path, s.status = u.Path, tasktypes.Status_CREATED
	return &tasks.CreateTaskResponse{ContainerID: req.ContainerID, Pid: 123}, nil
}

func (s *runtimeTasks) Start(_ context.Context, _ *tasks.StartRequest, _ ...grpc.CallOption) (*tasks.StartResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = tasktypes.Status_RUNNING
	if s.exitOnStart {
		s.status = tasktypes.Status_STOPPED
		close(s.exited)
	}
	return &tasks.StartResponse{Pid: 123}, errgrpc.ToGRPC(s.startFailure)
}

func (s *runtimeTasks) Get(_ context.Context, req *tasks.GetRequest, _ ...grpc.CallOption) (*tasks.GetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	return &tasks.GetResponse{Process: &tasktypes.Process{ID: req.ContainerID, Pid: 123, Status: s.status}}, nil
}

func (s *runtimeTasks) Wait(ctx context.Context, _ *tasks.WaitRequest, _ ...grpc.CallOption) (*tasks.WaitResponse, error) {
	select {
	case <-ctx.Done():
		return nil, errgrpc.ToGRPC(ctx.Err())
	case <-s.exited:
		return &tasks.WaitResponse{ExitStatus: 7}, nil
	}
}

func (s *runtimeTasks) Kill(_ context.Context, req *tasks.KillRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = append(s.signals, req.Signal)
	if s.status != tasktypes.Status_STOPPED && req.Signal == uint32(syscall.SIGKILL) {
		s.status = tasktypes.Status_STOPPED
		close(s.exited)
	}
	return &emptypb.Empty{}, nil
}

func (s *runtimeTasks) Delete(_ context.Context, _ *tasks.DeleteTaskRequest, _ ...grpc.CallOption) (*tasks.DeleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted {
		return nil, errgrpc.ToGRPC(errdefs.ErrNotFound)
	}
	if err := os.WriteFile(s.path, []byte("final shim output\n"), 0o600); err != nil {
		return nil, err
	}
	s.deleted = true
	return &tasks.DeleteResponse{ExitStatus: 7}, nil
}

func TestContainerdTaskLifecycle(t *testing.T) {
	for _, scenario := range []string{"fast exit", "graceful timeout", "lost start response", "restart recovery"} {
		t.Run(scenario, func(t *testing.T) {
			store := &runtimeStore{record: containers.Container{
				ID: "job", Snapshotter: "overlayfs", SnapshotKey: "job-rootfs",
				Spec:    &anypb.Any{Value: []byte(`{"root":{"path":"rootfs"}}`)},
				Labels:  map[string]string{"ebs.io/runner": "test", "ebs.io/job-uid": "job-uid"},
				Runtime: containers.RuntimeInfo{Name: "io.containerd.runc.v2"},
			}}
			snapshotter := &runtimeSnapshots{}
			taskService := &runtimeTasks{exited: make(chan struct{}), exitOnStart: scenario == "fast exit"}
			if scenario == "lost start response" {
				taskService.startFailure = context.DeadlineExceeded
			}
			client, err := containerd.New("", containerd.WithServices(containerd.WithContainerStore(store),
				containerd.WithTaskClient(taskService),
				containerd.WithSnapshotters(map[string]snapshots.Snapshotter{"overlayfs": snapshotter})))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			runtimeCtx, cancelRuntime := context.WithCancel(namespaces.WithNamespace(context.Background(), "eulermaker"))
			r := &Containerd{client: client, namespace: "eulermaker", logDir: t.TempDir(), ctx: runtimeCtx,
				cancel: cancelRuntime, tasks: make(map[string]*containerdExecution)}
			defer r.Close()
			ids, err := r.List(ctx, map[string]string{"ebs.io/runner": "test", "ebs.io/job-uid": ""})
			if err != nil || !reflect.DeepEqual(ids, []string{"job"}) {
				t.Fatalf("list = %v, %v", ids, err)
			}
			labels, err := r.Labels(ctx, "job")
			if err != nil || labels["ebs.io/runner"] != "test" {
				t.Fatalf("labels = %v, %v", labels, err)
			}
			if err := r.Start(ctx, "job"); !errors.Is(err, taskService.startFailure) {
				t.Fatalf("start = %v", err)
			}
			if scenario != "fast exit" {
				if scenario == "restart recovery" {
					// A new Runner has no attached task map; durable metadata is sufficient to stop it.
					recovered := &Containerd{client: client, namespace: "eulermaker"}
					err = recovered.Stop(ctx, "job", time.Millisecond)
				} else {
					err = r.Stop(ctx, "job", time.Millisecond)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if code, err := r.Wait(ctx, "job"); err != nil || code != 7 {
				t.Fatalf("wait = %d, %v", code, err)
			}
			var logs bytes.Buffer
			if err := r.Logs(ctx, "job", &logs); err != nil || logs.String() != "final shim output\n" {
				t.Fatalf("logs = %q, %v", logs.String(), err)
			}
			if err := r.Remove(ctx, "job"); err != nil {
				t.Fatal(err)
			}
			if !store.deleted || snapshotter.removed != "job-rootfs" || len(r.tasks) != 0 {
				t.Fatal("container metadata, snapshot or execution state remains")
			}
			if scenario != "fast exit" && !reflect.DeepEqual(taskService.signals, []uint32{uint32(syscall.SIGTERM), uint32(syscall.SIGKILL)}) {
				t.Fatalf("signals = %v", taskService.signals)
			}
		})
	}
}

// Opt in on a disposable Linux node with containerd, runc and image access.
func TestContainerdIntegration(t *testing.T) {
	address := os.Getenv("EBS_TEST_CONTAINERD_ADDRESS")
	if address == "" {
		t.Skip("set EBS_TEST_CONTAINERD_ADDRESS to run against a real daemon")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	namespace := "ebs-test-" + time.Now().Format("20060102150405.000000000")
	root := t.TempDir()
	r, err := NewContainerd(address, namespace, "overlayfs", root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := r.Remove(ctx, "job"); err != nil {
			t.Errorf("clean test container: %v", err)
		}
		if err := r.client.ImageService().Delete(r.context(ctx), "docker.io/library/busybox:latest", images.SynchronousDelete()); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("clean test image: %v", err)
		}
		if err := r.client.NamespaceService().Delete(ctx, namespace); err != nil {
			t.Errorf("clean test namespace: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Errorf("close test client: %v", err)
		}
	})
	if err := r.Pull(ctx, "busybox"); err != nil {
		t.Fatal(err)
	}
	if found, err := r.ImageExists(ctx, "busybox"); err != nil || !found {
		t.Fatalf("image exists = %v, %v", found, err)
	}
	id, err := r.Create(ctx, Spec{Name: "job", Image: "busybox", NetworkMode: "host",
		Command: []string{"sh", "-c", "printf stdout; printf stderr >&2; exit 7"},
		Labels:  map[string]string{"ebs.io/runner": "test", "ebs.io/job-uid": "test-job"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if code, err := r.Wait(ctx, id); err != nil || code != 7 {
		t.Fatalf("wait = %d, %v", code, err)
	}
	var output bytes.Buffer
	if err := r.Logs(ctx, id, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("stdout")) || !bytes.Contains(output.Bytes(), []byte("stderr")) {
		t.Fatalf("logs = %q", output.String())
	}
	c, err := r.client.LoadContainer(r.context(ctx), id)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(r.context(ctx))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := r.client.SnapshotService("overlayfs").Stat(r.context(ctx), info.SnapshotKey); err == nil {
		t.Fatal("task rootfs snapshot remains after removal")
	}

	id, err = r.Create(ctx, Spec{Name: "job", Image: "busybox", NetworkMode: "host",
		Command: []string{"sh", "-c", "trap '' TERM; echo ready; while :; do sleep 1; done"},
		Labels:  map[string]string{"ebs.io/runner": "test", "ebs.io/job-uid": "test-job"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	path, err := r.logPath(id)
	if err != nil {
		t.Fatal(err)
	}
	for {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("ready")) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewContainerd(address, namespace, "overlayfs", root)
	if err != nil {
		t.Fatal(err)
	}
	r = recovered
	ids, err := r.List(ctx, map[string]string{"ebs.io/runner": "test", "ebs.io/job-uid": ""})
	if err != nil || !reflect.DeepEqual(ids, []string{"job"}) {
		t.Fatalf("recovered containers = %v, %v", ids, err)
	}
	if err := r.Stop(ctx, id, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("runtime log remains after recovery: %v", err)
	}
}

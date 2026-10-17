package container

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/filters"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

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

package container

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/identifiers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/distribution/reference"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type Containerd struct {
	client      *containerd.Client
	namespace   string
	snapshotter string
	logDir      string
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	tasks       map[string]*containerdExecution
}

type containerdExecution struct {
	done chan struct{}
	code int
	err  error
}

var _ ManagedRuntime = (*Containerd)(nil)

func NewContainerd(address, namespace, snapshotter, rootDir string) (*Containerd, error) {
	if address == "" || snapshotter == "" || !filepath.IsAbs(rootDir) {
		return nil, fmt.Errorf("containerd requires an address, snapshotter and absolute root directory")
	}
	if err := identifiers.Validate(namespace); err != nil {
		return nil, fmt.Errorf("containerd namespace: %w", err)
	}
	client, err := containerd.New(address, containerd.WithTimeout(10*time.Second))
	if err != nil {
		return nil, fmt.Errorf("connect containerd: %w", err)
	}
	ctx, cancel := context.WithCancel(namespaces.WithNamespace(context.Background(), namespace))
	return &Containerd{
		client: client, namespace: namespace, snapshotter: snapshotter,
		logDir: filepath.Join(rootDir, "containerd", namespace), ctx: ctx, cancel: cancel,
		tasks: make(map[string]*containerdExecution),
	}, nil
}

func (r *Containerd) Close() error {
	r.cancel()
	return r.client.Close()
}

func (r *Containerd) context(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, r.namespace)
}

func imageReference(image string) (string, error) {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", err
	}
	return reference.TagNameOnly(ref).String(), nil
}

func (r *Containerd) ImageExists(ctx context.Context, image string) (bool, error) {
	name, err := imageReference(image)
	if err != nil {
		return false, err
	}
	_, err = r.client.GetImage(r.context(ctx), name)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (r *Containerd) Pull(ctx context.Context, image string) error {
	name, err := imageReference(image)
	if err != nil {
		return err
	}
	_, err = r.client.Pull(r.context(ctx), name, containerd.WithPullUnpack,
		containerd.WithPullSnapshotter(r.snapshotter))
	return err
}

func (r *Containerd) Create(ctx context.Context, spec Spec) (string, error) {
	if err := identifiers.Validate(spec.Name); err != nil {
		return "", err
	}
	// Host networking is the CT Job contract; bridge networking needs a CNI lifecycle.
	if spec.NetworkMode != "host" {
		return "", fmt.Errorf("containerd requires networkMode=host, got %q", spec.NetworkMode)
	}
	ctx = r.context(ctx)
	name, err := imageReference(spec.Image)
	if err != nil {
		return "", err
	}
	image, err := r.client.GetImage(ctx, name)
	if err != nil {
		return "", err
	}
	if err := image.Unpack(ctx, r.snapshotter); err != nil {
		return "", fmt.Errorf("unpack image: %w", err)
	}
	options, err := containerdSpecOptions(image, spec)
	if err != nil {
		return "", err
	}
	// NewContainer leases the snapshot until the container metadata references it.
	c, err := r.client.NewContainer(ctx, spec.Name, containerd.WithImage(image),
		containerd.WithRuntime("io.containerd.runc.v2", nil), containerd.WithContainerLabels(spec.Labels),
		containerd.WithSnapshotter(r.snapshotter), containerd.WithNewSnapshot(spec.Name+"-"+rand.Text(), image),
		containerd.WithNewSpec(options...))
	if err != nil {
		return "", err
	}
	return c.ID(), nil
}

func (r *Containerd) Start(ctx context.Context, id string) error {
	c, err := r.client.LoadContainer(r.context(ctx), id)
	if err != nil {
		return err
	}
	path, err := r.logPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.logDir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return err
	}
	// The shim writes this file even when Runner is disconnected or restarting.
	task, err := c.NewTask(r.context(ctx), cio.LogFile(path))
	if err != nil {
		return err
	}
	execution := &containerdExecution{done: make(chan struct{}), code: -1}
	r.mu.Lock()
	r.tasks[id] = execution
	r.mu.Unlock()
	// Subscribe before Start so even an immediately exiting process is observed.
	exits, err := task.Wait(r.ctx)
	if err != nil {
		execution.err = err
		close(execution.done)
		return err
	}
	go r.observeExit(task, exits, execution)
	return task.Start(r.context(ctx))
}

func (r *Containerd) observeExit(task containerd.Task, exits <-chan containerd.ExitStatus, execution *containerdExecution) {
	defer close(execution.done)
	select {
	case <-r.ctx.Done():
		execution.err = r.ctx.Err()
	case exit := <-exits:
		code, _, err := exit.Result()
		err = errgrpc.ToNative(err)
		execution.code, execution.err = int(code), err
		if err != nil {
			return
		}
		// Task deletion drains shim IO before Logs reads the final bytes. Keep the
		// container metadata/snapshot until the executor finishes log uploading.
		ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
		defer cancel()
		_, err = task.Delete(ctx)
		if err != nil && !errdefs.IsNotFound(err) {
			execution.err = fmt.Errorf("drain containerd task: %w", err)
		}
	}
}

func (r *Containerd) execution(id string) (*containerdExecution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	execution, ok := r.tasks[id]
	if !ok {
		return nil, fmt.Errorf("containerd task %q is not attached to this Runner process", id)
	}
	return execution, nil
}

func (r *Containerd) Wait(ctx context.Context, id string) (int, error) {
	execution, err := r.execution(id)
	if err != nil {
		return -1, err
	}
	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-execution.done:
		return execution.code, execution.err
	}
}

func (r *Containerd) Stop(ctx context.Context, id string, gracePeriod time.Duration) error {
	return r.stop(ctx, id, syscall.SIGTERM, gracePeriod)
}

func (r *Containerd) Kill(ctx context.Context, id string) error {
	return r.stop(ctx, id, syscall.SIGKILL, 0)
}

func (r *Containerd) stop(ctx context.Context, id string, signal syscall.Signal, grace time.Duration) error {
	ctx = r.context(ctx)
	c, err := r.client.LoadContainer(ctx, id)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	task, err := c.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exits, err := task.Wait(ctx)
	if err != nil {
		return err
	}
	status, err := task.Status(ctx)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if status.Status == containerd.Stopped {
		return nil
	}
	if status.Status == containerd.Created {
		signal = syscall.SIGKILL
	}
	if err := task.Kill(ctx, signal, containerd.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	var deadline <-chan time.Time
	if signal != syscall.SIGKILL {
		deadline = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case exit := <-exits:
			_, _, err := exit.Result()
			err = errgrpc.ToNative(err)
			if errdefs.IsNotFound(err) {
				return nil
			}
			return err
		case <-deadline:
			deadline = nil
			if err := task.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
				return err
			}
		}
	}
}

func (r *Containerd) Remove(ctx context.Context, id string) error {
	ctx = r.context(ctx)
	path, err := r.logPath(id)
	if err != nil {
		return err
	}
	c, err := r.client.LoadContainer(ctx, id)
	if err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	if err == nil {
		task, err := c.Task(ctx, nil)
		if err == nil {
			_, err = task.Delete(ctx, containerd.WithProcessKill)
		}
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		if err := c.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	r.mu.Lock()
	delete(r.tasks, id)
	r.mu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (r *Containerd) List(ctx context.Context, labels map[string]string) ([]string, error) {
	ctx = r.context(ctx)
	filters := make([]string, 0, len(labels))
	for key, value := range labels {
		filter := fmt.Sprintf("labels.%q", key)
		if value != "" {
			filter += fmt.Sprintf("==%q", value)
		}
		filters = append(filters, filter)
	}
	// Commas are AND; separate filter arguments are OR in containerd.
	containers, err := r.client.Containers(ctx, strings.Join(filters, ","))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(containers))
	for _, c := range containers {
		ids = append(ids, c.ID())
	}
	return ids, nil
}

func (r *Containerd) Labels(ctx context.Context, id string) (map[string]string, error) {
	ctx = r.context(ctx)
	c, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	return c.Labels(ctx)
}

func (r *Containerd) logPath(id string) (string, error) {
	if err := identifiers.Validate(id); err != nil {
		return "", err
	}
	return filepath.Join(r.logDir, id+".log"), nil
}

func (r *Containerd) Logs(ctx context.Context, id string, output io.Writer) error {
	execution, err := r.execution(id)
	if err != nil {
		return err
	}
	path, err := r.logPath(id)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return followContainerdLog(ctx, f, output, execution)
}

func followContainerdLog(ctx context.Context, input io.Reader, output io.Writer, execution *containerdExecution) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := input.Read(buf)
		if n > 0 {
			written, writeErr := output.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if err != nil && err != io.EOF {
			return err
		}
		if err != io.EOF {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-execution.done:
			// The shim has drained and closed its output; copy the tail exactly once.
			_, err := io.CopyBuffer(output, input, buf)
			if err != nil {
				return err
			}
			return execution.err
		case <-ticker.C:
		}
	}
}

func containerdSpecOptions(image oci.Image, spec Spec) ([]oci.SpecOpts, error) {
	args := append(append([]string(nil), spec.Command...), spec.Args...)
	opts := []oci.SpecOpts{
		oci.WithImageConfigArgs(image, args), oci.WithHostNamespace(specs.NetworkNamespace),
		oci.WithHostHostsFile, oci.WithHostResolvconf,
	}
	if spec.Entrypoint != "" {
		opts = append(opts, oci.WithProcessArgs(append([]string{spec.Entrypoint}, args...)...))
	}
	if spec.WorkingDir != "" {
		opts = append(opts, oci.WithProcessCwd(spec.WorkingDir))
	}
	env := make([]string, 0, len(spec.Env))
	for key, value := range spec.Env {
		env = append(env, key+"="+value)
	}
	sort.Strings(env)
	opts = append(opts, oci.WithEnv(env))
	mounts := make([]specs.Mount, 0, len(spec.Mounts))
	for host, target := range spec.Mounts {
		if !filepath.IsAbs(host) || !filepath.IsAbs(target) {
			return nil, fmt.Errorf("containerd bind mounts require absolute paths: %q -> %q", host, target)
		}
		mounts = append(mounts, specs.Mount{
			Source: host, Destination: filepath.Clean(target), Type: "bind", Options: []string{"rbind", "rprivate", "rw"},
		})
	}
	// Parents must be mounted before nested destinations.
	sort.Slice(mounts, func(i, j int) bool { return mounts[i].Destination < mounts[j].Destination })
	opts = append(opts, oci.WithMounts(mounts))
	if spec.Privileged {
		opts = append(opts, oci.WithPrivileged, oci.WithHostDevices, oci.WithAllDevicesAllowed)
	}
	// No seccomp profile is installed, matching Docker's current unconfined setting.
	return opts, nil
}

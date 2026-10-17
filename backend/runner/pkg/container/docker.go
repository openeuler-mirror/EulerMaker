package container

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/moby/moby/api/pkg/stdcopy"
	dockertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type Docker struct {
	client *client.Client
}

var _ ManagedRuntime = (*Docker)(nil)

func NewDocker() (*Docker, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Docker{client: cli}, nil
}

func (d *Docker) Close() error { return d.client.Close() }

func (d *Docker) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := d.client.ImageInspect(ctx, image)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (d *Docker) Pull(ctx context.Context, image string) error {
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return err
	}
	registry := reference.Domain(ref)
	if registry == "docker.io" {
		registry = "https://index.docker.io/v1/"
	}
	// Read Docker login credentials without requiring the Docker executable.
	config := dockerconfig.LoadDefaultConfigFile(os.Stderr)
	auth, err := config.GetAuthConfig(registry)
	if err != nil {
		return fmt.Errorf("read Docker registry credentials: %w", err)
	}
	encoded, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	response, err := d.client.ImagePull(ctx, image, client.ImagePullOptions{
		RegistryAuth: base64.URLEncoding.EncodeToString(encoded),
	})
	if err != nil {
		return err
	}
	defer response.Close()
	// A successful HTTP response can still contain a registry error in the stream.
	return response.Wait(ctx)
}

func (d *Docker) Remove(ctx context.Context, id string) error {
	_, err := d.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	return err
}

func (d *Docker) Create(ctx context.Context, spec Spec) (string, error) {
	config := &dockertypes.Config{
		Image: spec.Image, Labels: spec.Labels, WorkingDir: spec.WorkingDir,
	}
	config.Cmd = append(config.Cmd, spec.Command...)
	config.Cmd = append(config.Cmd, spec.Args...)
	if spec.Entrypoint != "" {
		config.Entrypoint = []string{spec.Entrypoint}
		// Match docker create --entrypoint: discard the image's default Cmd.
		if len(config.Cmd) == 0 {
			config.Cmd = []string{}
		}
	}
	for key, value := range spec.Env {
		config.Env = append(config.Env, key+"="+value)
	}
	sort.Strings(config.Env)
	host := &dockertypes.HostConfig{
		NetworkMode: dockertypes.NetworkMode(spec.NetworkMode),
		Privileged:  spec.Privileged,
		// Some ARM hosts deny clone3 with EPERM, preventing glibc's clone fallback.
		// The Runner's seccomp setting does not propagate to Job containers.
		// TODO: Remove once host profiles permit clone3 or return ENOSYS.
		SecurityOpt: []string{"seccomp=unconfined"},
	}
	for source, target := range spec.Mounts {
		host.Binds = append(host.Binds, source+":"+target)
	}
	sort.Strings(host.Binds)
	response, err := d.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: spec.Name, Config: config, HostConfig: host,
	})
	if err != nil {
		return "", err
	}
	if response.ID == "" {
		return "", fmt.Errorf("Docker create returned empty container id")
	}
	return response.ID, nil
}

func (d *Docker) Start(ctx context.Context, id string) error {
	_, err := d.client.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (d *Docker) Logs(ctx context.Context, id string, output io.Writer) error {
	inspect, err := d.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	stream, err := d.client.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true,
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	if inspect.Container.Config != nil && inspect.Container.Config.Tty {
		_, err = io.Copy(output, stream)
	} else {
		_, err = stdcopy.StdCopy(output, output, stream)
	}
	return err
}

func (d *Docker) Wait(ctx context.Context, id string) (int, error) {
	wait := d.client.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: dockertypes.WaitConditionNotRunning})
	select {
	case result := <-wait.Result:
		if result.Error != nil && result.Error.Message != "" {
			return -1, fmt.Errorf("Docker wait: %s", result.Error.Message)
		}
		return int(result.StatusCode), nil
	case err := <-wait.Error:
		return -1, err
	}
}

func (d *Docker) Stop(ctx context.Context, id string, gracePeriod time.Duration) error {
	seconds := max(1, int(gracePeriod.Seconds()))
	_, err := d.client.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &seconds})
	return err
}

func (d *Docker) Kill(ctx context.Context, id string) error {
	_, err := d.client.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: "SIGKILL"})
	return err
}

// List returns container IDs matching every label, including stopped containers.
func (d *Docker) List(ctx context.Context, labels map[string]string) ([]string, error) {
	filters := make(client.Filters)
	for key, value := range labels {
		label := key
		if value != "" {
			label += "=" + value
		}
		filters.Add("label", label)
	}
	response, err := d.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(response.Items))
	for _, item := range response.Items {
		ids = append(ids, item.ID)
	}
	return ids, nil
}

func (d *Docker) Labels(ctx context.Context, id string) (map[string]string, error) {
	response, err := d.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	if response.Container.Config == nil {
		return nil, fmt.Errorf("Docker inspect returned no container config")
	}
	return response.Container.Config.Labels, nil
}

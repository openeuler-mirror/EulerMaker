package container

import (
	"context"
	"io"
	"time"
)

type Spec struct {
	Name        string
	Image       string
	Privileged  bool
	NetworkMode string
	WorkingDir  string
	Env         map[string]string
	Mounts      map[string]string
	Labels      map[string]string
	Entrypoint  string
	Command     []string
	Args        []string
}

type Runtime interface {
	ImageExists(ctx context.Context, image string) (bool, error)
	Pull(ctx context.Context, image string) error
	Remove(ctx context.Context, name string) error
	Create(ctx context.Context, spec Spec) (string, error)
	Start(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, output io.Writer) error
	Wait(ctx context.Context, id string) (int, error)
	Stop(ctx context.Context, id string, gracePeriod time.Duration) error
	Kill(ctx context.Context, id string) error
}

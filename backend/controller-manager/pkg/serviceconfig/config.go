package serviceconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

// Provider reads current service configuration and supplies address-isolated Git clients.
type Provider interface {
	Load(context.Context) (*ebsv1.ServiceConfigContent, error)
	GitClient(string) (gitserver.GitServerClient, error)
}

type Getter interface {
	Get(context.Context, schema.GroupVersionResource, string, string) (runtime.Object, error)
}

type provider struct {
	client    Getter
	gitConfig gitserver.Config
	mu        sync.Mutex
	address   string
	git       gitserver.GitServerClient
}

// New does no I/O; controllers load configuration only when their round needs a service.
func New(client Getter, config gitserver.Config) Provider {
	return &provider{client: client, gitConfig: config}
}

// Error separates service configuration failures from package or repository failures.
type Error struct {
	Err       error
	permanent bool
}

func (e *Error) Error() string   { return "service configuration: " + e.Err.Error() }
func (e *Error) Unwrap() error   { return e.Err }
func (e *Error) Permanent() bool { return e.permanent }

func IsError(err error) bool {
	var target *Error
	return errors.As(err, &target)
}

func invalid(format string, args ...any) error {
	return &Error{Err: fmt.Errorf(format, args...), permanent: true}
}

func (p *provider) Load(ctx context.Context) (*ebsv1.ServiceConfigContent, error) {
	obj, err := p.client.Get(ctx, source.ConfigsGVR, "", ebsv1.ServiceConfigName)
	if err != nil {
		permanent := true
		var status apierrors.APIStatus
		var network net.Error
		if errors.As(err, &status) {
			code := status.Status().Code
			permanent = code >= 400 && code < 500 && code != 404 && code != 408 && code != 429
		} else if errors.As(err, &network) || errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			permanent = false
		}
		return nil, &Error{Err: err, permanent: permanent}
	}
	config, ok := obj.(*ebsv1.Config)
	if !ok || config == nil || config.Name != ebsv1.ServiceConfigName || config.Namespace != "" ||
		config.UID == "" || config.ResourceVersion == "" || config.Spec.Visibility != ebsv1.ConfigVisibilityOpsOnly {
		return nil, invalid("unexpected Config response: %T", obj)
	}
	var content ebsv1.ServiceConfigContent
	if err := yaml.UnmarshalStrict([]byte(config.Spec.Content), &content); err != nil {
		return nil, invalid("decode Config/%s: %v", ebsv1.ServiceConfigName, err)
	}
	return &content, nil
}

func (p *provider) GitClient(address string) (gitserver.GitServerClient, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.git != nil && address == p.address {
		return p.git, nil
	}
	config := p.gitConfig
	config.Address = address
	client, err := gitserver.New(config)
	if err != nil {
		return nil, invalid("create Git Server client: %v", err)
	}
	// Old clients remain alive only while in-flight rounds hold them.
	p.address, p.git = address, client
	return client, nil
}

// Round pins a single configuration snapshot and Git client, including for concurrent package workers.
type Round struct {
	provider Provider
	once     sync.Once
	content  *ebsv1.ServiceConfigContent
	err      error
	gitOnce  sync.Once
	git      gitserver.GitServerClient
	gitErr   error
}

func NewRound(provider Provider) *Round { return &Round{provider: provider} }

func (r *Round) load(ctx context.Context) (*ebsv1.ServiceConfigContent, error) {
	r.once.Do(func() {
		r.content, r.err = r.provider.Load(ctx)
		if r.err == nil && r.content == nil {
			r.err = invalid("empty service configuration response")
		}
	})
	return r.content, r.err
}

func (r *Round) GitClient(ctx context.Context) (gitserver.GitServerClient, error) {
	r.gitOnce.Do(func() {
		content, err := r.load(ctx)
		if err != nil {
			r.gitErr = err
			return
		}
		if content.GitServer.InternalURL == "" {
			r.gitErr = invalid("Git Server address is missing")
			return
		}
		r.git, r.gitErr = r.provider.GitClient(content.GitServer.InternalURL)
	})
	return r.git, r.gitErr
}

func (r *Round) ArtifactURL(ctx context.Context, instance string) (string, error) {
	content, err := r.load(ctx)
	if err != nil {
		return "", err
	}
	address := content.ArtifactManagers[instance].InternalURL
	if address == "" {
		return "", invalid("artifact manager instance %q has no management address", instance)
	}
	return address, nil
}

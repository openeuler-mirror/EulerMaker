package rpmrepo

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/clock"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/serviceconfig"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
)

const Name = "rpmrepo"

const (
	// nonTerminalRpmRepoFieldSelector keeps the polling source on objects that still need repository or release
	// work. The field selector only supports Equals/NotEquals, and a missing release field is not excluded.
	nonTerminalRpmRepoFieldSelector = "status.release.phase!=Ready,status.release.phase!=Failed,status.release.phase!=Skipped"

	// defaultPollAfterSeconds is used when Artifact Manager omits pollAfterSeconds or returns a non-positive one.
	defaultPollAfterSeconds = 5 * time.Second
)

// Backoff carries the framework slow retry parameters. They drive the materialization backoff curve and the
// controller slow retry wrapper without being duplicated inside Config.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Jitter  float64
}

func (b Backoff) validate() error {
	if b.Initial <= 0 || b.Max < b.Initial || b.Jitter < 0 || b.Jitter >= 1 {
		return fmt.Errorf("materialization backoff delays must be positive, ordered and jitter must be in [0, 1)")
	}
	return nil
}

type Config struct {
	ArtifactManagerTimeout time.Duration
	MaxJobsPerBatch        int
	MaterializeRetryLimit  int
	PollPeriod             time.Duration
	MaxRetries             int
	Backoff                Backoff
}

func (c Config) validate() error {
	if c.ArtifactManagerTimeout <= 0 {
		return fmt.Errorf("artifact manager timeout must be positive")
	}
	if c.MaxJobsPerBatch <= 0 || c.MaterializeRetryLimit <= 0 {
		return fmt.Errorf("batch size and materialize retry limit must be positive")
	}
	if c.PollPeriod <= 0 || c.MaxRetries < 0 {
		return fmt.Errorf("poll period must be positive and the retry count must not be negative")
	}
	return c.Backoff.validate()
}

type Controller struct {
	*controller.BaseController
	rpmrepos        source.Source
	client          Client
	artifacts       ArtifactManagerClient
	artifactFactory func(string) (ArtifactManagerClient, error)
	services        serviceconfig.Provider
	policy          PublishPolicy
	clock           clock.Clock
	config          Config
}

// New wires the non-terminal RpmRepo stream; Initializer adds deletion polling for repositories and BuildInfos.
func New(
	rpmrepos source.Source,
	client Client,
	artifacts ArtifactManagerClient,
	policy PublishPolicy,
	clk clock.Clock,
	config Config,
	options ...controller.Option,
) (*Controller, error) {
	return newController(rpmrepos, client, artifacts, nil, policy, clk, config, options...)
}

func newController(
	rpmrepos source.Source, client Client, artifacts ArtifactManagerClient,
	factory func(string) (ArtifactManagerClient, error), policy PublishPolicy, clk clock.Clock,
	config Config, options ...controller.Option,
) (*Controller, error) {
	if rpmrepos == nil || client == nil || (artifacts == nil && factory == nil) || policy == nil || clk == nil {
		return nil, fmt.Errorf("RpmRepo source, API client, Artifact Manager client, publish policy and clock are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	c := &Controller{rpmrepos: rpmrepos, client: client, artifacts: artifacts, artifactFactory: factory,
		policy: policy, clock: clk, config: config}
	base, err := controller.New(Name, c.sync, config.MaxRetries, options...)
	if err != nil {
		return nil, err
	}
	c.BaseController = base
	if err := rpmrepos.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: c.onAdd, UpdateFunc: c.onUpdate, DeleteFunc: c.onDelete}); err != nil {
		return nil, fmt.Errorf("register RpmRepo handler: %w", err)
	}
	return c, nil
}

func Initializer(config Config, services serviceconfig.Provider) manager.InitFunc {
	return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
		if services == nil {
			return nil, false, fmt.Errorf("service configuration provider is required")
		}
		if err := config.validate(); err != nil {
			return nil, false, err
		}
		rpmrepos, err := init.Dependencies.PollingFactory.ForResource(
			source.RpmReposGVR,
			config.PollPeriod,
			metav1.ListOptions{FieldSelector: nonTerminalRpmRepoFieldSelector},
		)
		if err != nil {
			return nil, false, err
		}
		factory := func(address string) (ArtifactManagerClient, error) {
			return newArtifactManagerClient(address, config.ArtifactManagerTimeout)
		}
		value, err := newController(rpmrepos, newAPIClient(init.Dependencies.Client), nil, factory, DefaultPublishPolicy{}, clock.RealClock{}, config,
			controller.WithSlowRetry(init.Config.SlowRetryInitial, init.Config.SlowRetryMax, init.Config.SlowRetryJitter))
		if err == nil {
			value.services = services
		}
		if err != nil {
			return nil, false, err
		}

		for _, gvr := range []schema.GroupVersionResource{source.BuildInfosGVR, source.RpmReposGVR} {
			deleting, err := init.Dependencies.PollingFactory.ForResource(gvr, config.PollPeriod,
				metav1.ListOptions{FieldSelector: "metadata.deletionTimestamp!=null"})
			if err != nil {
				return nil, false, err
			}
			enqueue := func(obj runtime.Object) {
				metadata, err := meta.Accessor(obj)
				if err == nil && metadata.GetNamespace() != "" && metadata.GetName() != "" {
					value.Enqueue(rpmRepoKey(metadata.GetNamespace(), metadata.GetName()))
				}
			}
			if err := deleting.AddEventHandler(source.ResourceEventHandlerFuncs{AddFunc: enqueue, UpdateFunc: func(_, obj runtime.Object) { enqueue(obj) }}); err != nil {
				return nil, false, err
			}
		}
		return value, err == nil, err
	}
}

func (c *Controller) onAdd(obj runtime.Object) { c.enqueueRpmRepo(obj) }

func (c *Controller) onUpdate(_, newObj runtime.Object) { c.enqueueRpmRepo(newObj) }

// onDelete only logs: an object leaving the polling snapshot does not mean it was deleted.
func (c *Controller) onDelete(obj runtime.Object) {
	repo, ok := obj.(*ebsv1.RpmRepo)
	if !ok || repo == nil || repo.Name == "" || repo.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedRpmRepoEvent type=%T", Name, obj)
		return
	}
	log.Printf("controller=%s key=%q uid=%q reason=RpmRepoLeftPollingSnapshot", Name, rpmRepoKey(repo.Namespace, repo.Name), repo.UID)
}

func (c *Controller) enqueueRpmRepo(obj runtime.Object) {
	repo, ok := obj.(*ebsv1.RpmRepo)
	if !ok || repo == nil || repo.Name == "" || repo.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedRpmRepoEvent type=%T", Name, obj)
		return
	}
	if repo.DeletionTimestamp == nil && releaseTerminal(repo) {
		return
	}
	c.Enqueue(rpmRepoKey(repo.Namespace, repo.Name))
}

func rpmRepoKey(project, name string) string {
	return project + "/" + name
}

// releaseTerminal reports a business terminal state; deletion cleanup still runs for these repositories.
func releaseTerminal(repo *ebsv1.RpmRepo) bool {
	if repo == nil || repo.Status.Release == nil {
		return false
	}
	phase := repo.Status.Release.Phase
	return phase == ebsv1.RpmRepoReleaseReady || phase == ebsv1.RpmRepoReleaseFailed || phase == ebsv1.RpmRepoReleaseSkipped
}

// splitKey parses the single RpmRepo queue key: project/buildName.
func splitKey(value string) (project, name string, ok bool) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

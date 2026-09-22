package rpmrepo

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"controller-manager/pkg/controller"
	"controller-manager/pkg/manager"
	"controller-manager/pkg/source"
	ebsv1 "ebs-api/ebs/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
)

const Name = "rpmrepo"

const (
	buildKeyPrefix   = "build"
	releaseKeyPrefix = "release"

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
	ArtifactManagerAddr    string
	ArtifactManagerTimeout time.Duration
	MaxJobsPerBatch        int
	MaxInputBytes          int64
	MaterializeRetryLimit  int
	PollPeriod             time.Duration
	MaxRetries             int
	Backoff                Backoff
}

func (c Config) validate() error {
	if strings.TrimSpace(c.ArtifactManagerAddr) == "" {
		return fmt.Errorf("artifact manager address is required")
	}
	parsed, err := url.Parse(c.ArtifactManagerAddr)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("artifact manager address must be an absolute http or https URL")
	}
	if c.ArtifactManagerTimeout <= 0 {
		return fmt.Errorf("artifact manager timeout must be positive")
	}
	if c.MaxJobsPerBatch <= 0 || c.MaxInputBytes <= 0 || c.MaterializeRetryLimit <= 0 {
		return fmt.Errorf("batch size, input byte limit and materialize retry limit must be positive")
	}
	if c.PollPeriod <= 0 || c.MaxRetries < 0 {
		return fmt.Errorf("poll period must be positive and the retry count must not be negative")
	}
	return c.Backoff.validate()
}

type Controller struct {
	*controller.BaseController
	rpmrepos  source.Source
	client    Client
	artifacts ArtifactManagerClient
	policy    PublishPolicy
	clock     clock.Clock
	config    Config
}

// New wires one RpmRepo controller. The source is expected to deliver the non-terminal RpmRepo polling stream;
// every key is derived from it, so no Job or Build event source is registered.
func New(
	rpmrepos source.Source,
	client Client,
	artifacts ArtifactManagerClient,
	policy PublishPolicy,
	clk clock.Clock,
	config Config,
	options ...controller.Option,
) (*Controller, error) {
	if rpmrepos == nil || client == nil || artifacts == nil || policy == nil || clk == nil {
		return nil, fmt.Errorf("RpmRepo source, API client, Artifact Manager client, publish policy and clock are required")
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	c := &Controller{rpmrepos: rpmrepos, client: client, artifacts: artifacts, policy: policy, clock: clk, config: config}
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

func Initializer(config Config) manager.InitFunc {
	return func(_ context.Context, init manager.InitContext) (controller.Controller, bool, error) {
		// M1 keeps the controller inactive until an Artifact Manager is configured, so a deployment that has
		// not adopted the RpmRepo controller yet still starts with the default --controllers=* selection.
		if strings.TrimSpace(config.ArtifactManagerAddr) == "" {
			log.Printf("controller=%s result=inactive reason=ArtifactManagerNotConfigured", Name)
			return nil, false, nil
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
		artifacts, err := newArtifactManagerClient(config.ArtifactManagerAddr, config.ArtifactManagerTimeout)
		if err != nil {
			return nil, false, err
		}
		value, err := New(rpmrepos, newAPIClient(init.Dependencies.Client), artifacts, DefaultPublishPolicy{}, clock.RealClock{}, config,
			controller.WithSlowRetry(init.Config.SlowRetryInitial, init.Config.SlowRetryMax, init.Config.SlowRetryJitter))
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
	log.Printf("controller=%s key=%q uid=%q reason=RpmRepoLeftPollingSnapshot", Name, buildKey(repo.Namespace, repo.Name), repo.UID)
}

func (c *Controller) enqueueRpmRepo(obj runtime.Object) {
	repo, ok := obj.(*ebsv1.RpmRepo)
	if !ok || repo == nil || repo.Name == "" || repo.Namespace == "" {
		log.Printf("controller=%s reason=UnexpectedRpmRepoEvent type=%T", Name, obj)
		return
	}
	if repo.DeletionTimestamp != nil || releaseTerminal(repo) {
		return
	}
	c.Enqueue(buildKey(repo.Namespace, repo.Name))
}

func buildKey(project, name string) string {
	return buildKeyPrefix + "/" + project + "/" + name
}

func releaseKey(project, os, arch string) string {
	return releaseKeyPrefix + "/" + project + "/" + os + "/" + arch
}

// releaseTerminal reports whether the formal release already reached a terminal phase. Skipped is written by
// the Build Controller for single builds and is terminal for this controller too: it must never be advanced,
// rewritten on abort, or counted.
func releaseTerminal(repo *ebsv1.RpmRepo) bool {
	if repo == nil || repo.Status.Release == nil {
		return false
	}
	phase := repo.Status.Release.Phase
	return phase == ebsv1.RpmRepoReleaseReady || phase == ebsv1.RpmRepoReleaseFailed || phase == ebsv1.RpmRepoReleaseSkipped
}

// splitKey parses a queue key into its kind, project and remaining segments.
func splitKey(value string) (kind, project string, rest []string, ok bool) {
	parts := strings.Split(value, "/")
	if len(parts) < 3 {
		return "", "", nil, false
	}
	kind, project = parts[0], parts[1]
	if kind != buildKeyPrefix && kind != releaseKeyPrefix {
		return "", "", nil, false
	}
	if project == "" {
		return "", "", nil, false
	}
	rest = parts[2:]
	for _, item := range rest {
		if item == "" {
			return "", "", nil, false
		}
	}
	switch kind {
	case buildKeyPrefix:
		if len(rest) != 1 {
			return "", "", nil, false
		}
	case releaseKeyPrefix:
		if len(rest) != 2 {
			return "", "", nil, false
		}
	}
	return kind, project, rest, true
}

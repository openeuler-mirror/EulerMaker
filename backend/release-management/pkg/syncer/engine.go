package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"time"

	ebsv1 "ebs-api/ebs/v1"
)

type Plan struct {
	Binding         string     `json:"binding"`
	ProjectUID      string     `json:"projectUID"`
	ResourceVersion string     `json:"resourceVersion"`
	SourceCommit    string     `json:"sourceCommit"`
	ContentDigest   string     `json:"contentDigest"`
	Difference      Difference `json:"difference"`
	Blocked         string     `json:"blocked,omitempty"`
	list            []ebsv1.PackageRepo
	patch           []byte
}

type Status struct {
	Binding         string     `json:"binding"`
	ProjectUID      string     `json:"projectUID"`
	ConfigDigest    string     `json:"configDigest"`
	Phase           string     `json:"phase"`
	SourceCommit    string     `json:"sourceCommit,omitempty"`
	ContentDigest   string     `json:"contentDigest,omitempty"`
	LastSuccess     *time.Time `json:"lastSuccess,omitempty"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	Error           string     `json:"error,omitempty"`
	Added           int        `json:"added"`
	Removed         int        `json:"removed"`
	Modified        int        `json:"modified"`
	Attempts        uint64     `json:"attempts"`
	Failures        uint64     `json:"failures"`
	DurationSeconds float64    `json:"durationSeconds"`
	QueueSeconds    float64    `json:"queueSeconds"`
}

type bindingState struct {
	binding Binding
	lock    chan struct{}
	wake    chan struct{}
	mu      sync.Mutex
	status  Status
}

type Engine struct {
	config      Config
	reader      SourceReader
	client      ProjectClient
	bindings    map[string]*bindingState
	sources     map[string]Source
	slots       chan struct{}
	writeRecord func(string, Status) error
}

func NewEngine(c Config, reader SourceReader, client ProjectClient) *Engine {
	e := &Engine{
		config: c, reader: reader, client: client,
		bindings: map[string]*bindingState{}, sources: map[string]Source{},
		slots: make(chan struct{}, c.Workers), writeRecord: persistRecord,
	}
	for _, s := range c.Sources {
		e.sources[s.Name] = s
	}
	for _, b := range c.Bindings {
		state := &bindingState{
			binding: b, lock: make(chan struct{}, 1), wake: make(chan struct{}, 1),
			status: Status{Binding: b.Name, ConfigDigest: digest(c), Phase: "Queued"},
		}
		// Persisted records are audit hints only; startup always rechecks Git and Project.
		if data, err := readRecord(filepath.Join(c.DataDir, "bindings", b.Name+".json")); err == nil {
			var saved Status
			if json.Unmarshal(data, &saved) == nil && saved.Binding == b.Name && saved.ConfigDigest == state.status.ConfigDigest {
				state.status = saved
				state.status.Phase = "Queued"
			}
		}
		e.bindings[b.Name] = state
	}
	return e
}

func (e *Engine) acquire(ctx context.Context, state *bindingState) (func(), error) {
	select {
	case state.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, retry("binding wait canceled")
	}
	select {
	case e.slots <- struct{}{}:
		return func() { <-e.slots; <-state.lock }, nil
	case <-ctx.Done():
		<-state.lock
		return nil, retry("worker wait canceled")
	}
}

func (e *Engine) Plan(ctx context.Context, name string) (*Plan, error) {
	state, exists := e.bindings[name]
	if !exists {
		return nil, blocked("unknown binding")
	}
	ctx, cancel := context.WithTimeout(ctx, roundTimeout)
	defer cancel()
	unlock, err := e.acquire(ctx, state)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return e.prepare(ctx, state.binding)
}

func (e *Engine) prepare(ctx context.Context, b Binding) (*Plan, error) {
	commit, files, err := e.reader.Read(ctx, e.sources[b.Source], b.Paths)
	if err != nil {
		return nil, err
	}
	project, err := e.client.Get(ctx, b)
	if err != nil {
		return nil, err
	}
	if err := checkProject(b, project, ""); err != nil {
		return nil, err
	}
	list, err := buildList(b, files, project.Spec.DefaultRef)
	if err != nil {
		return nil, blocked(err.Error())
	}
	old, err := normalize(project.Spec.PackageRepos)
	if err != nil {
		return nil, blocked("existing Project package list is invalid")
	}
	plan := &Plan{
		Binding: b.Name, ProjectUID: project.Metadata.UID, ResourceVersion: project.Metadata.ResourceVersion,
		SourceCommit: commit, ContentDigest: digest(list), Difference: difference(old, list), list: list,
	}
	if len(list) == 0 {
		plan.Blocked = "empty candidate list is blocked"
		return plan, blocked(plan.Blocked)
	}
	plan.patch, err = preflight(project, list, e.config.MaxProjectBytes)
	if err != nil {
		plan.Blocked = err.Error()
		return plan, err
	}
	return plan, nil
}

func checkProject(b Binding, p *Project, expectedUID string) error {
	if p == nil || p.Metadata.Name != b.Project || p.Metadata.UID == "" || p.Metadata.ResourceVersion == "" {
		return blocked("Project response identity is invalid")
	}
	if p.Metadata.DeletionTimestamp != nil {
		return blocked("Project is deleting")
	}
	if expectedUID != "" && p.Metadata.UID != expectedUID {
		return retry("Project was recreated during synchronization; recompute required")
	}
	return nil
}

func (e *Engine) apply(ctx context.Context, b Binding, p *Plan) error {
	if len(p.Difference.Changes) == 0 {
		return nil
	}
	result, err := e.client.Patch(ctx, b, p.patch)
	if err == nil && checkProject(b, result, p.ProjectUID) == nil && equalList(result.Spec.PackageRepos, p.list) {
		return nil
	}
	if err != nil {
		failure := failureOf(err)
		if failure.Status == 403 {
			current, getErr := e.client.Get(ctx, b)
			if getErr != nil {
				return getErr
			}
			if checkErr := checkProject(b, current, p.ProjectUID); checkErr != nil {
				return checkErr
			}
			if current.Metadata.ResourceVersion != p.ResourceVersion {
				return retry("Project version changed; recompute required")
			}
			return blocked("Project update permission denied")
		}
		if failure.Blocked || failure.Status == 409 || failure.Status == 429 {
			return err
		}
	}
	// An interrupted write or anomalous success may already have committed. Confirm once, without another PATCH.
	current, confirmErr := e.client.Get(ctx, b)
	if confirmErr != nil {
		return confirmErr
	}
	if checkErr := checkProject(b, current, p.ProjectUID); checkErr != nil {
		return checkErr
	}
	if equalList(current.Spec.PackageRepos, p.list) {
		return nil
	}
	if err != nil {
		return err
	}
	return retry("Project update result unconfirmed")
}

func (e *Engine) Queue(name string) error {
	state, exists := e.bindings[name]
	if !exists {
		return blocked("unknown binding")
	}
	select {
	case state.wake <- struct{}{}:
	default:
	}
	return nil
}

func (e *Engine) Status(name string) (Status, error) {
	state, exists := e.bindings[name]
	if !exists {
		return Status{}, blocked("unknown binding")
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.status, nil
}

func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, state := range e.bindings {
		wg.Add(1)
		go func() { defer wg.Done(); e.loop(ctx, state) }()
	}
	wg.Wait()
}

func (e *Engine) loop(ctx context.Context, state *bindingState) {
	interval, _ := time.ParseDuration(e.config.PollInterval)
	timer := time.NewTimer(0)
	defer timer.Stop()
	var notBefore time.Time
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-state.wake:
		}
		if ctx.Err() != nil {
			return
		}
		if remaining := time.Until(notBefore); remaining > 0 {
			timer.Reset(remaining)
			continue
		}
		// Coalesce all signals received before this round; one new signal during it marks the binding dirty.
		select {
		case <-state.wake:
		default:
		}
		err := e.sync(ctx, state)
		if ctx.Err() != nil {
			return
		}
		delay := jitter(interval)
		notBefore = time.Time{}
		if err != nil && !failureOf(err).Blocked {
			attempt++
			delay = retryDelay(attempt, failureOf(err).RetryAfter)
			notBefore = time.Now().Add(delay)
		} else {
			attempt = 0
		}
		timer.Reset(delay)
	}
}

func jitter(d time.Duration) time.Duration {
	return d + time.Duration(float64(d)*0.1*rand.Float64())
}

func retryDelay(attempt int, serverDelay time.Duration) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempt && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	d = jitter(d)
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	if serverDelay > d {
		d = serverDelay
	}
	return d
}

func (e *Engine) sync(ctx context.Context, state *bindingState) error {
	ctx, cancel := context.WithTimeout(ctx, roundTimeout)
	defer cancel()
	queued := time.Now()
	state.mu.Lock()
	state.status.Phase = "Queued"
	state.mu.Unlock()
	unlock, err := e.acquire(ctx, state)
	if err != nil {
		return err
	}
	defer unlock()
	start := time.Now()
	state.mu.Lock()
	state.status.Phase = "Syncing"
	previous := state.status
	state.mu.Unlock()
	plan, err := e.prepare(ctx, state.binding)
	if err == nil {
		err = e.apply(ctx, state.binding, plan)
	}
	next := previous
	next.Attempts++
	next.UpdatedAt = time.Now().UTC()
	next.DurationSeconds += time.Since(start).Seconds()
	next.QueueSeconds += start.Sub(queued).Seconds()
	next.Added, next.Removed, next.Modified = 0, 0, 0
	if plan != nil {
		next.Added, next.Removed, next.Modified = plan.Difference.Added, plan.Difference.Removed, plan.Difference.Modified
	}
	if err == nil {
		next.Phase, next.Error = "Idle", ""
		next.ProjectUID = plan.ProjectUID
		next.SourceCommit, next.ContentDigest = plan.SourceCommit, plan.ContentDigest
		now := next.UpdatedAt
		next.LastSuccess = &now
	} else {
		next.Failures++
		next.Phase, next.Error = "Retrying", failureOf(err).Message
		if failureOf(err).Blocked {
			next.Phase = "Blocked"
		}
	}
	if e.writeRecord(filepath.Join(e.config.DataDir, "bindings", state.binding.Name+".json"), next) != nil {
		if err == nil {
			next.Failures++
		}
		err = retry("write local synchronization record failed")
		next.Phase, next.Error = "Retrying", err.Error()
		next.ProjectUID = previous.ProjectUID
		next.LastSuccess, next.SourceCommit, next.ContentDigest = previous.LastSuccess, previous.SourceCommit, previous.ContentDigest
	}
	state.mu.Lock()
	state.status = next
	state.mu.Unlock()
	attributes := []any{"binding", state.binding.Name, "project", state.binding.Project, "result", next.Phase, "duration", time.Since(start), "added", next.Added, "removed", next.Removed, "modified", next.Modified}
	if plan != nil {
		attributes = append(attributes, "commit", plan.SourceCommit)
	}
	if err != nil {
		if previous.Error != next.Error || next.Attempts%10 == 0 {
			slog.Warn("synchronization failed", append(attributes, "cause", next.Error)...)
		}
	} else if len(plan.Difference.Changes) > 0 {
		slog.Info("Project synchronized", attributes...)
	} else {
		slog.Debug("Project unchanged", attributes...)
	}
	return err
}

func persistRecord(filename string, record Status) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err := json.NewEncoder(f).Encode(record); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filename); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readRecord(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if len(data) > 64<<10 {
		return nil, fmt.Errorf("record exceeds size limit")
	}
	return data, err
}

func (e *Engine) metrics() string {
	var result string
	for _, b := range e.config.Bindings {
		s, _ := e.Status(b.Name)
		last := int64(0)
		if s.LastSuccess != nil {
			last = s.LastSuccess.Unix()
		}
		for metric, value := range map[string]float64{
			"attempts_total": float64(s.Attempts), "failures_total": float64(s.Failures),
			"last_success_timestamp_seconds": float64(last), "duration_seconds_total": s.DurationSeconds,
			"queue_seconds_total": s.QueueSeconds,
		} {
			result += fmt.Sprintf("release_management_%s{binding=%q} %g\n", metric, b.Name, value)
		}
	}
	return result
}

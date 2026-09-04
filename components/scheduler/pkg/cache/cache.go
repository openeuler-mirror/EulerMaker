package cache

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	ebsv1 "ebs-api/ebs/v1"
	"k8s.io/apimachinery/pkg/types"
	"scheduler/pkg/framework"
)

var ErrStaleSnapshot = errors.New("stale scheduler snapshot")

type Interface interface {
	Snapshot(jobKey string, uid types.UID) (*framework.Snapshot, error)
	Assume(AssumeRequest) (uint64, error)
	Forget(jobKey string, uid types.UID, generation uint64) bool
	GetAssumed(jobKey string, uid types.UID) (*AssumedJob, bool)
	ClaimExpiredAssumed(now time.Time, retryAfter time.Duration, limit int) []*AssumedJob
}

type AssumeRequest struct {
	JobKey             string
	JobUID             types.UID
	RunnerName         string
	RunnerUID          types.UID
	RunnerRevision     uint64
	Requests           framework.Resource
	JobResourceVersion string
}

type AssumedJob struct {
	AssumeRequest
	Generation  uint64
	AssumedAt   time.Time
	NextCheckAt time.Time
}

type jobState struct {
	job      *ebsv1.Job
	requests framework.Resource
	invalid  error
}

type runnerState struct {
	runner      *ebsv1.Runner
	allocatable framework.Resource
	invalid     error
	revision    uint64
}

type SchedulerCache struct {
	mu sync.RWMutex

	jobs            map[string]*jobState
	runners         map[string]*runnerState
	runningUsage    map[string]framework.Resource
	runningJobCount map[string]int64
	runningInvalid  map[string]int64
	assumedByJob    map[string]*AssumedJob
	assumedByRunner map[string]map[string]*AssumedJob
	nextGeneration  uint64
	now             func() time.Time
	assumeTimeout   time.Duration
}

func New(assumeTimeout ...time.Duration) *SchedulerCache {
	timeout := 30 * time.Second
	if len(assumeTimeout) > 0 {
		timeout = assumeTimeout[0]
	}
	return &SchedulerCache{
		jobs: map[string]*jobState{}, runners: map[string]*runnerState{},
		runningUsage: map[string]framework.Resource{}, runningJobCount: map[string]int64{}, runningInvalid: map[string]int64{},
		assumedByJob: map[string]*AssumedJob{}, assumedByRunner: map[string]map[string]*AssumedJob{},
		now: time.Now, assumeTimeout: timeout,
	}
}

func JobKey(job *ebsv1.Job) string { return job.Namespace + "/" + job.Name }

func schedulable(job *ebsv1.Job) bool {
	return job != nil && job.Status.Phase == "Pending" && job.Status.Runner == ""
}
func running(job *ebsv1.Job) bool {
	return job != nil && job.Status.Phase == "Running" && job.Status.Runner != ""
}

func (c *SchedulerCache) UpsertJob(job *ebsv1.Job) {
	if job == nil {
		return
	}
	copy := job.DeepCopy()
	requests, invalid := framework.ParseRequests(copy.Spec.Resources)
	key := JobKey(copy)
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.jobs[key]
	changed := old == nil || old.job.UID != copy.UID || old.job.Status.Phase != copy.Status.Phase || old.job.Status.Runner != copy.Status.Runner || !old.requests.Equal(requests) || !sameError(old.invalid, invalid)
	if old != nil && changed {
		c.removeRunningLocked(old)
	}
	c.jobs[key] = &jobState{job: copy, requests: requests, invalid: invalid}
	if changed {
		c.addRunningLocked(c.jobs[key])
	}
	if assumed := c.assumedByJob[key]; assumed != nil {
		if assumed.JobUID != copy.UID || (copy.Status.Runner != "" && copy.Status.Runner != assumed.RunnerName) || !schedulable(copy) {
			c.forgetLocked(assumed)
		}
	}
}

func (c *SchedulerCache) DeleteJob(key string, uid types.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	old := c.jobs[key]
	if old == nil || old.job.UID != uid {
		return
	}
	c.removeRunningLocked(old)
	delete(c.jobs, key)
	if assumed := c.assumedByJob[key]; assumed != nil && assumed.JobUID == uid {
		c.forgetLocked(assumed)
	}
}

func (c *SchedulerCache) addRunningLocked(state *jobState) {
	if !running(state.job) {
		return
	}
	name := state.job.Status.Runner
	c.runningUsage[name] = c.runningUsage[name].Add(state.requests)
	c.runningJobCount[name]++
	if state.invalid != nil {
		c.runningInvalid[name]++
	}
	if runner := c.runners[name]; runner != nil {
		runner.revision++
	}
}

func (c *SchedulerCache) removeRunningLocked(state *jobState) {
	if !running(state.job) {
		return
	}
	name := state.job.Status.Runner
	c.runningUsage[name] = c.runningUsage[name].Sub(state.requests)
	c.runningJobCount[name]--
	if state.invalid != nil {
		c.runningInvalid[name]--
	}
	if c.runningJobCount[name] <= 0 {
		delete(c.runningJobCount, name)
	}
	if c.runningInvalid[name] <= 0 {
		delete(c.runningInvalid, name)
	}
	if runner := c.runners[name]; runner != nil {
		runner.revision++
	}
}

func (c *SchedulerCache) UpsertRunner(runner *ebsv1.Runner) {
	if runner == nil {
		return
	}
	copy := runner.DeepCopy()
	allocatable, invalid := framework.ParseAllocatable(copy.Status.Allocatable)
	c.mu.Lock()
	defer c.mu.Unlock()
	revision := uint64(1)
	if old := c.runners[copy.Name]; old != nil {
		revision = old.revision
		if runnerSchedulingChanged(old.runner, copy) || !old.allocatable.Equal(allocatable) || !sameError(old.invalid, invalid) {
			revision++
		}
	}
	c.runners[copy.Name] = &runnerState{runner: copy, allocatable: allocatable, invalid: invalid, revision: revision}
}

func runnerSchedulingChanged(a, b *ebsv1.Runner) bool {
	return a.UID != b.UID || !reflect.DeepEqual(a.Labels, b.Labels) || !reflect.DeepEqual(a.Spec, b.Spec) || a.Status.Phase != b.Status.Phase
}
func sameError(a, b error) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func (c *SchedulerCache) DeleteRunner(name string, uid types.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.runners[name]; old != nil && old.runner.UID == uid {
		delete(c.runners, name)
	}
}

func (c *SchedulerCache) Snapshot(jobKey string, uid types.UID) (*framework.Snapshot, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	job := c.jobs[jobKey]
	if job == nil || job.job.UID != uid || !schedulable(job.job) {
		return nil, ErrStaleSnapshot
	}
	result := &framework.Snapshot{Job: job.job.DeepCopy(), Requests: job.requests.DeepCopy(), Invalid: job.invalid, Runners: map[string]*framework.RunnerSnapshot{}, CreatedAt: c.now()}
	names := make([]string, 0, len(c.runners))
	for name := range c.runners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		runner := c.runners[name]
		assumedUsage := framework.Resource{}
		for _, assumed := range c.assumedByRunner[name] {
			assumedUsage = assumedUsage.Add(assumed.Requests)
		}
		available := runner.allocatable.Sub(c.runningUsage[name]).Sub(assumedUsage).ClampZero()
		invalid := runner.invalid
		if invalid == nil && c.runningInvalid[name] > 0 {
			invalid = fmt.Errorf("runner has Running Job with invalid resource requests")
		}
		result.Runners[name] = &framework.RunnerSnapshot{Runner: runner.runner.DeepCopy(), Allocatable: runner.allocatable.DeepCopy(), Available: available, RunningJobCount: c.runningJobCount[name], AssumedJobCount: int64(len(c.assumedByRunner[name])), Revision: runner.revision, Invalid: invalid}
	}
	return result, nil
}

func (c *SchedulerCache) Assume(request AssumeRequest) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	job := c.jobs[request.JobKey]
	runner := c.runners[request.RunnerName]
	if job == nil || job.job.UID != request.JobUID || job.job.ResourceVersion != request.JobResourceVersion || !schedulable(job.job) || runner == nil || runner.runner.UID != request.RunnerUID || runner.revision != request.RunnerRevision || runner.invalid != nil {
		return 0, ErrStaleSnapshot
	}
	if old := c.assumedByJob[request.JobKey]; old != nil {
		if old.JobUID == request.JobUID && old.RunnerUID == request.RunnerUID && old.RunnerRevision == request.RunnerRevision && old.JobResourceVersion == request.JobResourceVersion && old.Requests.Equal(request.Requests) {
			return old.Generation, nil
		}
		return 0, ErrStaleSnapshot
	}
	used := c.runningUsage[request.RunnerName]
	for _, assumed := range c.assumedByRunner[request.RunnerName] {
		used = used.Add(assumed.Requests)
	}
	if !runner.allocatable.Sub(used).Fits(request.Requests) {
		return 0, ErrStaleSnapshot
	}
	c.nextGeneration++
	now := c.now()
	assumed := &AssumedJob{AssumeRequest: request, Generation: c.nextGeneration, AssumedAt: now, NextCheckAt: now.Add(c.assumeTimeout)}
	c.assumedByJob[request.JobKey] = assumed
	if c.assumedByRunner[request.RunnerName] == nil {
		c.assumedByRunner[request.RunnerName] = map[string]*AssumedJob{}
	}
	c.assumedByRunner[request.RunnerName][request.JobKey] = assumed
	return assumed.Generation, nil
}

func (c *SchedulerCache) Forget(key string, uid types.UID, generation uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.assumedByJob[key]
	if a == nil || a.JobUID != uid || a.Generation != generation {
		return false
	}
	c.forgetLocked(a)
	return true
}
func (c *SchedulerCache) forgetLocked(a *AssumedJob) {
	delete(c.assumedByJob, a.JobKey)
	delete(c.assumedByRunner[a.RunnerName], a.JobKey)
	if len(c.assumedByRunner[a.RunnerName]) == 0 {
		delete(c.assumedByRunner, a.RunnerName)
	}
}

func cloneAssumed(a *AssumedJob) *AssumedJob {
	if a == nil {
		return nil
	}
	copy := *a
	copy.Requests = a.Requests.DeepCopy()
	return &copy
}
func (c *SchedulerCache) GetAssumed(key string, uid types.UID) (*AssumedJob, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	a := c.assumedByJob[key]
	if a == nil || a.JobUID != uid {
		return nil, false
	}
	return cloneAssumed(a), true
}
func (c *SchedulerCache) ClaimExpiredAssumed(now time.Time, retryAfter time.Duration, limit int) []*AssumedJob {
	if limit <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.assumedByJob))
	for key := range c.assumedByJob {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]*AssumedJob, 0, limit)
	for _, key := range keys {
		a := c.assumedByJob[key]
		if len(result) == limit {
			break
		}
		if a.NextCheckAt.IsZero() || !a.NextCheckAt.After(now) {
			a.NextCheckAt = now.Add(retryAfter)
			result = append(result, cloneAssumed(a))
		}
	}
	return result
}

func (c *SchedulerCache) AssumedCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.assumedByJob)
}

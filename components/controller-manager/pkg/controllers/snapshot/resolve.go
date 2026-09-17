package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"sync"
	"time"

	"controller-manager/pkg/clients/gitserver"
	"controller-manager/pkg/controller"
	ebsv1 "ebs-api/ebs/v1"
)

type resolveState string

const (
	stateResolved resolveState = "Resolved"
	stateWaiting  resolveState = "Waiting"
	stateDeferred resolveState = "Deferred"
	stateFailed   resolveState = "Failed"
	stateSkipped  resolveState = "Skipped"
	stateFatal    resolveState = "Fatal"
)

type resolveTask struct {
	index int
	repo  ebsv1.PackageRepo
}

type resolveResult struct {
	index           int
	repo            ebsv1.PackageRepo
	state           resolveState
	status          ebsv1.PackageRepoStatus
	fatal           error
	unexpectedError bool
}

var fullCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var errResolveBudget = errors.New("snapshot resolve budget exhausted")

func (c *Controller) resolveAll(parent context.Context, snapshot *ebsv1.Snapshot, targets []ebsv1.PackageRepo) ([]resolveResult, error) {
	started := c.clock.Now()
	defer func() {
		resolveBatches.Inc()
		if elapsed := c.clock.Now().Sub(started); elapsed > 0 {
			resolveDuration.Add(uint64(elapsed))
		}
	}()
	results := make([]resolveResult, 0, len(targets))
	nameCount := make(map[string]int)
	for _, repo := range targets {
		nameCount[repo.Name]++
	}
	tasks := make([]resolveTask, 0, len(targets))
	indices := make(map[string]int, len(snapshot.Spec.PackageRepos))
	for index, repo := range snapshot.Spec.PackageRepos {
		indices[repo.Name] = index
	}
	for _, repo := range targets {
		index := indices[repo.Name]
		repo = effectiveRepo(repo, snapshot.Spec.DefaultRef)
		if nameCount[repo.Name] > 1 {
			results = append(results, skippedResult(index, repo, ebsv1.SpecCommitValidationFailed, "duplicate package repository name"))
			continue
		}
		if status, ok := snapshot.Status.PackageRepoStatuses[repo.Name]; ok && (status.CommitID != "" || (status.Error != nil && !status.Error.Retryable)) {
			c.failures.set(snapshot.UID, repo.Name, 0)
			continue
		}
		tasks = append(tasks, resolveTask{index: index, repo: repo})
	}
	if len(tasks) == 0 {
		sortResults(results)
		return results, nil
	}
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].index < tasks[j].index })
	budgetCtx, cancel := context.WithTimeoutCause(parent, c.config.ResolveBudget, errResolveBudget)
	defer cancel()
	// Rotate dispatch order, while preserving stable indices for merging.
	cursor := c.failures.cursor(snapshot.UID)
	pivot := sort.Search(len(tasks), func(i int) bool { return tasks[i].index >= cursor })
	tasks = append(append(make([]resolveTask, 0, len(tasks)), tasks[pivot:]...), tasks[:pivot]...)
	jobs := make(chan resolveTask, len(tasks))
	resultCh := make(chan resolveResult, len(tasks))
	for _, task := range tasks {
		jobs <- task
	}
	close(jobs)
	workers := c.config.ResolveWorkers
	if workers > len(tasks) {
		workers = len(tasks)
	}
	var wg sync.WaitGroup
	var dispatchMu sync.Mutex
	nextCursor := cursor
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// Serialize receipt and cursor accounting, never external requests.
				dispatchMu.Lock()
				if budgetCtx.Err() != nil {
					dispatchMu.Unlock()
					return
				}
				task, ok := <-jobs
				if !ok {
					dispatchMu.Unlock()
					return
				}
				nextCursor = (task.index + 1) % len(snapshot.Spec.PackageRepos)
				dispatchMu.Unlock()
				resultCh <- c.resolveOne(budgetCtx, task, snapshot.CreationTimestamp.Time)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(resultCh)
		close(done)
	}()
	received := make(map[int]struct{}, len(tasks))
	for result := range resultCh {
		results = append(results, result)
		received[result.index] = struct{}{}
	}
	<-done
	c.failures.setCursor(snapshot.UID, nextCursor)
	if parent.Err() != nil {
		return nil, parent.Err()
	}
	for _, task := range tasks {
		if _, ok := received[task.index]; !ok {
			results = append(results, resolveResult{index: task.index, repo: task.repo, state: stateDeferred})
		}
	}
	sortResults(results)
	for _, result := range results {
		if result.unexpectedError {
			unexpectedGitErrors.Inc()
		}
		if result.state == stateFatal {
			return nil, result.fatal
		}
		switch result.state {
		case stateResolved:
			resolvedPackages.Inc()
		case stateWaiting:
			waitingPackages.Inc()
		case stateDeferred:
			deferredPackages.Inc()
		case stateFailed:
			failedPackages.Inc()
		}
	}
	return results, nil
}

func (c *Controller) resolveOne(ctx context.Context, task resolveTask, baseline time.Time) resolveResult {
	if err := ctx.Err(); err != nil {
		return c.requestFailure(ctx, task, "sync", err)
	}
	repo := task.repo
	if repo.Ref.Type == "" || repo.Ref.Value == "" {
		return skippedResult(task.index, repo, ebsv1.SpecCommitValidationFailed, "repository ref and snapshot defaultRef do not provide a complete ref")
	}
	if repo.Ref.Type == ebsv1.GitRefCommit {
		if !fullCommitPattern.MatchString(repo.Ref.Value) {
			return skippedResult(task.index, repo, ebsv1.SpecCommitValidationFailed, "commit must be a full lowercase SHA")
		}
		return resolveResult{index: task.index, repo: repo, state: stateResolved, status: ebsv1.PackageRepoStatus{CommitID: repo.Ref.Value}}
	}
	check, err := c.gitClient.CheckSynced(ctx, repo.URL, baseline)
	if err != nil {
		return c.requestFailure(ctx, task, "sync", err)
	}
	if err := ctx.Err(); err != nil {
		return c.requestFailure(ctx, task, "sync", err)
	}
	if !check.Synced {
		if err := c.gitClient.PublishSyncTask(ctx, repo.URL); err != nil {
			return c.requestFailure(ctx, task, "sync", err)
		}
		return resolveResult{index: task.index, repo: repo, state: stateWaiting}
	}
	commit, err := c.gitClient.ResolveCommit(ctx, repo.URL, repo.Ref)
	if err != nil {
		result := c.requestFailure(ctx, task, "resolve", err)
		result.status.CloneURL = check.CloneURL
		return result
	}
	return resolveResult{index: task.index, repo: repo, state: stateResolved, status: ebsv1.PackageRepoStatus{CloneURL: check.CloneURL, CommitID: commit}}
}

// Only an interrupted request is deferred; a completed HTTP failure retains its
// classification even if the batch deadline expires at the same time.
func (c *Controller) requestFailure(ctx context.Context, task resolveTask, operation string, err error) resolveResult {
	if ctx.Err() != nil && (errors.Is(err, ctx.Err()) || isTimeout(err)) {
		if errors.Is(context.Cause(ctx), errResolveBudget) {
			return resolveResult{index: task.index, repo: task.repo, state: stateDeferred}
		}
		return resolveResult{index: task.index, repo: task.repo, state: stateFatal, fatal: ctx.Err()}
	}
	return c.gitFailure(task, operation, err)
}

func (c *Controller) gitFailure(task resolveTask, operation string, err error) (out resolveResult) {
	if errors.Is(err, context.Canceled) && err != context.DeadlineExceeded {
		return resolveResult{index: task.index, repo: task.repo, state: stateFatal, fatal: err}
	}
	kind, classified := gitErrorKind(err)
	defer func() { out.unexpectedError = !classified && !isTimeout(err) }()
	switch kind {
	case gitserver.ErrorValidation:
		return skippedResult(task.index, task.repo, ebsv1.SpecCommitValidationFailed, "git-server rejected repository input")
	case gitserver.ErrorNotFound:
		return skippedResult(task.index, task.repo, ebsv1.SpecCommitResolveFailed, "branch or tag was not found")
	case gitserver.ErrorPermanent:
		return resolveResult{index: task.index, repo: task.repo, state: stateFatal, fatal: controllerPermanent(err)}
	default:
		if isTimeout(err) {
			return failedResult(task.index, task.repo, ebsv1.SpecCommitSyncTimeout, operation+" request timed out")
		}
		code := ebsv1.SpecCommitSyncFailed
		if operation == "resolve" {
			code = ebsv1.SpecCommitResolveFailed
		}
		return failedResult(task.index, task.repo, code, operation+" request failed")
	}
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout())
}

func (c *Controller) mergeResults(snapshot *ebsv1.Snapshot, results []resolveResult) ([]trackerChange, bool, bool) {
	if snapshot.Status.PackageRepoStatuses == nil {
		snapshot.Status.PackageRepoStatuses = make(map[string]ebsv1.PackageRepoStatus)
	}
	accepted := make(map[string]string)
	for _, repo := range snapshot.Spec.PackageRepos {
		if status := snapshot.Status.PackageRepoStatuses[repo.Name]; status.CommitID != "" {
			accepted[repoIdentity(effectiveRepo(repo, snapshot.Spec.DefaultRef))] = status.CommitID
		}
	}
	changes := make([]trackerChange, 0, len(results))
	waiting, failed := false, false
	for _, result := range results {
		name := result.repo.Name
		switch result.state {
		case stateResolved:
			identity := repoIdentity(result.repo)
			if commit, ok := accepted[identity]; ok && commit != result.status.CommitID {
				cloneURL := result.status.CloneURL
				result = skippedResult(result.index, result.repo, ebsv1.SpecCommitCommitConflict, "repository ref resolved to conflicting commits")
				result.status.CloneURL = cloneURL
			} else {
				accepted[identity] = result.status.CommitID
				snapshot.Status.PackageRepoStatuses[name] = result.status
				changes = append(changes, trackerChange{repo: name})
				continue
			}
			fallthrough
		case stateSkipped:
			snapshot.Status.PackageRepoStatuses[name] = result.status
			changes = append(changes, trackerChange{repo: name})
		case stateDeferred:
			waiting = true
		case stateWaiting:
			waiting = true
			if old, ok := snapshot.Status.PackageRepoStatuses[name]; ok && old.Error != nil && old.Error.Retryable {
				delete(snapshot.Status.PackageRepoStatuses, name)
			}
			changes = append(changes, trackerChange{repo: name})
		case stateFailed:
			next := c.failures.count(snapshot.UID, name) + 1
			if next >= c.config.FailureLimit {
				result.status.Error = &ebsv1.SpecCommitError{Code: ebsv1.SpecCommitRetryExhausted, Message: "temporary git-server failures exhausted retry budget", Retryable: false}
				changes = append(changes, trackerChange{repo: name})
			} else {
				failed = true
				changes = append(changes, trackerChange{repo: name, count: next})
			}
			snapshot.Status.PackageRepoStatuses[name] = result.status
		}
	}
	return changes, waiting, failed
}

func skippedResult(index int, repo ebsv1.PackageRepo, code ebsv1.SpecCommitErrorCode, message string) resolveResult {
	return resolveResult{index: index, repo: repo, state: stateSkipped, status: ebsv1.PackageRepoStatus{Error: &ebsv1.SpecCommitError{Code: code, Message: sanitize(message), Retryable: false}}}
}

func failedResult(index int, repo ebsv1.PackageRepo, code ebsv1.SpecCommitErrorCode, message string) resolveResult {
	return resolveResult{index: index, repo: repo, state: stateFailed, status: ebsv1.PackageRepoStatus{Error: &ebsv1.SpecCommitError{Code: code, Message: sanitize(message), Retryable: true}}}
}

func sortResults(results []resolveResult) {
	sort.Slice(results, func(i, j int) bool { return results[i].index < results[j].index })
}

func repoIdentity(repo ebsv1.PackageRepo) string {
	key, err := gitserver.RepositoryKey(repo.URL)
	if err != nil {
		key = "invalid:" + repo.URL
	}
	return key + "\x00" + string(repo.Ref.Type) + "\x00" + repo.Ref.Value
}

// effectiveRepo resolves the frozen Snapshot default without changing spec inputs.
func effectiveRepo(repo ebsv1.PackageRepo, defaultRef ebsv1.GitRef) ebsv1.PackageRepo {
	if repo.Ref == (ebsv1.GitRef{}) {
		repo.Ref = defaultRef
	}
	return repo
}

func controllerPermanent(err error) error {
	return controller.NewPermanentError(fmt.Errorf("git-server permanent error: %w", err))
}
